package lwm2m

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/edgenesis/shifu/pkg/deviceshifu/deviceshifulwm2m/lwm2m"
	"github.com/edgenesis/shifu/pkg/k8s/api/v1alpha1"
	"github.com/edgenesis/shifu/pkg/logger"
	piondtls "github.com/pion/dtls/v2"
	"github.com/plgd-dev/go-coap/v3/dtls"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"
	"github.com/plgd-dev/go-coap/v3/options"
	"github.com/plgd-dev/go-coap/v3/udp"
	udpClient "github.com/plgd-dev/go-coap/v3/udp/client"
)

type Client struct {
	ctx context.Context
	Config

	locationPath     string
	updateInterval   int
	liftTime         int
	object           Object
	lastModifiedTime time.Time
	lastUpdatedTime  time.Time
	dataCache        map[string]interface{}

	conn *udpClient.Conn
	tmgr *TaskManager
}

type Config struct {
	EndpointName string
	EndpointUrl  string
	ShifuHost    string
	Settings     v1alpha1.LwM2MSettings
}

const (
	DefaultLifeTime       = 300
	DefaultUpdateInterval = 60
)

func NewClient(config Config) (*Client, error) {
	var client = &Client{
		ctx:            context.TODO(),
		Config:         config,
		liftTime:       DefaultLifeTime,
		updateInterval: DefaultUpdateInterval,
		object:         *NewObject("root", nil),
		tmgr:           NewTaskManager(),
		dataCache:      make(map[string]interface{}),
	}

	return client, nil
}

func (c *Client) Start() error {
	udpClientOpts := []udp.Option{}

	udpClientOpts = append(udpClientOpts,
		options.WithMux(c.handleRouter()),
	)

	var conn *udpClient.Conn
	var err error
	cipherSuites, err := lwm2m.StringsToCodes(c.Settings.CipherSuites)
	if err != nil {
		return err
	}
	switch *c.Settings.SecurityMode {
	case v1alpha1.SecurityModeDTLS:
		switch *c.Settings.DTLSMode {
		case v1alpha1.DTLSModePSK:
			dtlsConfig := &piondtls.Config{
				PSK: func(hint []byte) ([]byte, error) {
					fmt.Printf("Server's hint: %s \n", hint)
					return hex.DecodeString(*c.Settings.PSKKey)
				},
				PSKIdentityHint: []byte(*c.Settings.PSKIdentity),
				CipherSuites:    cipherSuites,
			}

			conn, err = dtls.Dial(c.EndpointUrl, dtlsConfig, udpClientOpts...)
		}
	default:
		fallthrough
	case v1alpha1.SecurityModeNone:
		conn, err = udp.Dial(c.EndpointUrl, udpClientOpts...)
	}
	if err != nil {
		return err
	}

	c.conn = conn
	return nil
}

func (c *Client) Object() Object {
	return c.object
}

func (c *Client) Register() error {
	coRELinkStr := c.object.GetCoRELinkString()
	request, err := c.conn.NewPostRequest(context.TODO(), "/rd", message.AppLinkFormat, strings.NewReader(coRELinkStr))
	if err != nil {
		return err
	}

	request.AddQuery("ep=" + c.EndpointName)
	request.AddQuery(fmt.Sprintf("lt=%d", c.liftTime))
	request.AddQuery("lwm2m=1.0")
	request.AddQuery("b=U")
	request.SetAccept(message.TextPlain)
	resp, err := c.conn.Do(request)
	if err != nil {
		return err
	}

	if resp.Code() != codes.Created {
		return errors.New("register failed")
	}

	locationPath, err := resp.Options().LocationPath()
	if err != nil {
		return err
	}

	c.locationPath = locationPath
	c.lastUpdatedTime = time.Now()
	go func() {
		panic(c.AutoUpdate())
	}()
	logger.Infof("register %v success", c.locationPath)
	return nil
}

func (c *Client) Delete() error {
	request, err := c.conn.NewDeleteRequest(context.Background(), c.locationPath)
	if err != nil {
		return err
	}

	resp, err := c.conn.Do(request)
	if err != nil {
		return err
	}

	if resp.Code() != codes.Deleted {
		return errors.New("delete failed")
	}

	logger.Infof("delete %v success", c.locationPath)
	return nil
}

func (c *Client) AutoUpdate() error {
	ticker := time.NewTicker(time.Duration(c.updateInterval) * time.Second)
	for {
		select {
		case <-c.ctx.Done():
			return nil
		case <-ticker.C:
			if c.isActivity() {
				if err := c.Update(); err != nil {
					logger.Errorf("failed to update registration: %v", err)
					continue
				}
				logger.Debug("update registration success")
			}
		}
	}
}

func (c *Client) Update() error {
	var coRELinkStr string
	// if have changed of the object should set the CoRELinkStr updated in payload
	if c.lastUpdatedTime.Before(c.lastModifiedTime) {
		coRELinkStr = c.object.GetCoRELinkString()
	} else {
		logger.Info("no data changed")
	}

	resp, err := c.conn.Post(context.TODO(), c.locationPath, message.AppLinkFormat, strings.NewReader(coRELinkStr))
	if err != nil {
		return err
	}

	if resp.Code() != codes.Changed {
		return errors.New("update failed")
	}

	c.lastUpdatedTime = time.Now()
	return nil
}

func (c *Client) handleRouter() *mux.Router {
	router := mux.NewRouter()
	router.DefaultHandle(mux.HandlerFunc(func(w mux.ResponseWriter, r *mux.Message) {
		if r.Type() == message.Reset {
			c.tmgr.CancelAllTasks()
			return
		}

		objectId, err := r.Path()
		if err != nil {
			_ = w.SetResponse(codes.BadRequest, message.TextPlain, strings.NewReader(err.Error()))
		}

		object := c.object.GetChildObject(objectId)
		if object == nil {
			_ = w.SetResponse(codes.NotFound, message.TextPlain, nil)
			return
		}

		switch r.Code() {
		case codes.GET:
			if r.Options().HasOption(message.Observe) {
				c.handleObserve(w, r)
				return
			}

			res, err := c.object.ReadAll(objectId)
			if err != nil {
				logger.Errorf("failed to read data from object, error: %v", err)
				_ = w.SetResponse(codes.NotFound, message.TextPlain, strings.NewReader(err.Error()))
				return
			}
			_ = w.SetResponse(codes.Content, message.AppLwm2mJSON, strings.NewReader(res.ReadAsJSON()))
			return
		case codes.PUT:
			newData, err := io.ReadAll(r.Body())
			if err != nil {
				_ = w.SetResponse(codes.BadRequest, message.TextPlain, strings.NewReader(err.Error()))
				return
			}
			err = object.Write(string(newData))
			if err != nil {
				_ = w.SetResponse(codes.BadRequest, message.TextPlain, strings.NewReader(err.Error()))
				return
			}
			_ = w.SetResponse(codes.Changed, message.TextPlain, nil)

		case codes.POST:
			err = object.Execute()
			if err != nil {
				_ = w.SetResponse(codes.BadRequest, message.TextPlain, strings.NewReader(err.Error()))
				return
			}

			_ = w.SetResponse(codes.Changed, message.TextPlain, nil)

		default:
			_ = w.SetResponse(codes.MethodNotAllowed, message.TextPlain, nil)
		}

	}))

	return router
}

func (c *Client) AddObject(object Object) {
	logger.Infof("add object %v", object.Id)
	if obj, exists := c.object.Child[object.Id]; exists {
		obj.AddObject(object.Id, object)
	} else {
		c.object.AddGroup(object)
	}

	c.lastModifiedTime = time.Now()
}

func (c *Client) Ping() error {
	return c.conn.Ping(c.ctx)
}

func (c *Client) handleObserve(w mux.ResponseWriter, r *mux.Message) {
	objectId, err := r.Path()
	if err != nil {
		_ = w.SetResponse(codes.BadRequest, message.TextPlain, strings.NewReader(err.Error()))
		return
	}

	logger.Debugf("observe %v", objectId)
	token := r.Token()
	var obs uint32 = 2
	c.tmgr.AddTask(objectId, time.Second*10, func() {
		data, err := c.object.ReadAll(objectId)
		if err != nil {
			return
		}

		jsonData := data.ReadAsJSON()

		c.dataCache[objectId] = string(jsonData)
		err = sendResponse(w.Conn(), token, obs, jsonData)
		if err != nil {
			return
		}
		obs++
		c.tmgr.ResetTask(objectId + "-ob")
	})

	c.tmgr.AddTask(objectId+"-ob", time.Second*5, func() {
		data, err := c.object.ReadAll(objectId)
		if err != nil {
			return
		}

		jsonData := data.ReadAsJSON()

		// check data is changed
		if data, exists := c.dataCache[objectId]; exists {
			if string(jsonData) == data {
				logger.Debug("no data changed")
				return
			}
		}

		c.dataCache[objectId] = string(jsonData)
		err = sendResponse(w.Conn(), token, obs, jsonData)
		if err != nil {
			return
		}
		obs++
		c.tmgr.ResetTask(objectId)
	})

	res, err := c.object.ReadAll(objectId)
	if err != nil {
		_ = w.SetResponse(codes.NotFound, message.TextPlain, nil)
		return
	}

	jsonData := res.ReadAsJSON()
	c.dataCache[objectId] = string(jsonData)
	_ = w.SetResponse(codes.Content, message.AppLwm2mJSON, strings.NewReader(jsonData),
		message.Option{ID: message.Observe, Value: []byte{byte(obs)}},
	)
}

func sendResponse(cc mux.Conn, token []byte, obs uint32, body string) error {
	m := cc.AcquireMessage(cc.Context())
	defer cc.ReleaseMessage(m)
	m.SetCode(codes.Content)
	m.SetToken(token)
	m.SetBody(strings.NewReader(body))
	m.SetContentFormat(message.AppLwm2mJSON)
	m.SetObserve(obs)
	return cc.WriteMessage(m)
}

func (c *Client) CleanUp() {
	c.tmgr.CancelAllTasks()
	_ = c.Delete()
}

func (c *Client) isActivity() bool {
	return time.Now().Before(c.lastUpdatedTime.Add(time.Duration(c.liftTime) * time.Second))
}
