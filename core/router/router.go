// Copyright © 2017 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package router

import (
	"sync"
	"time"

	pb_broker "github.com/TheThingsNetwork/api/broker"
	"github.com/TheThingsNetwork/api/broker/brokerclient"
	pb_discovery "github.com/TheThingsNetwork/api/discovery"
	pb_gateway "github.com/TheThingsNetwork/api/gateway"
	pb "github.com/TheThingsNetwork/api/router"
	"github.com/TheThingsNetwork/go-utils/grpc/auth"
	"github.com/TheThingsNetwork/go-utils/grpc/ttnctx"
	"github.com/TheThingsNetwork/ttn/core/component"
	"github.com/TheThingsNetwork/ttn/core/router/gateway"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// Router component
type Router interface {
	component.Interface
	component.ManagementInterface

	// Handle a status message from a gateway
	HandleGatewayStatus(gatewayID string, status *pb_gateway.Status) error
	// Handle an uplink message from a gateway
	HandleUplink(gatewayID string, uplink *pb.UplinkMessage) error
	// Handle a downlink message
	HandleDownlink(message *pb_broker.DownlinkMessage) error
	// Subscribe to downlink messages
	SubscribeDownlink(gatewayID string, subscriptionID string) (<-chan *pb.DownlinkMessage, error)
	// Unsubscribe from downlink messages
	UnsubscribeDownlink(gatewayID string, subscriptionID string) error
	// Handle a device activation
	HandleActivation(gatewayID string, activation *pb.DeviceActivationRequest) (*pb.DeviceActivationResponse, error)

	getGateway(gatewayID string) *gateway.Gateway
}

type broker struct {
	conn        *grpc.ClientConn
	association brokerclient.RouterStream
	client      pb_broker.BrokerClient
	uplink      chan *pb_broker.UplinkMessage
	downlink    chan *pb_broker.DownlinkMessage
}

// NewRouter creates a new Router
func NewRouter() Router {
	return &router{
		gateways: make(map[string]*gateway.Gateway),
		brokers:  make(map[string]*broker),
	}
}

type router struct {
	*component.Component
	gateways     map[string]*gateway.Gateway
	gatewaysLock sync.RWMutex
	brokers      map[string]*broker
	brokersLock  sync.RWMutex
	status       *status
	// monitorStream monitorclient.Stream
}

func (r *router) tickGateways() {
	r.gatewaysLock.RLock()
	defer r.gatewaysLock.RUnlock()
	for _, gtw := range r.gateways {
		gtw.Utilization.Tick()
	}
}

func (r *router) Init(c *component.Component) error {
	r.Component = c
	r.InitStatus()
	err := r.Component.UpdateTokenKey()
	if err != nil {
		return err
	}
	err = r.Component.Announce()
	if err != nil {
		return err
	}
	r.Discovery.GetAll("broker") // Update cache

	go func() {
		for range time.Tick(5 * time.Second) {
			r.tickGateways()
		}
	}()
	r.Component.SetStatus(component.StatusHealthy)
	// if r.Component.Monitor != nil {
	// 	r.monitorStream = r.Component.Monitor.RouterClient(r.Context, grpc.PerRPCCredentials(auth.WithStaticToken(r.AccessToken)))
	// }
	return nil
}

func (r *router) Shutdown() {
	r.brokersLock.Lock()
	defer r.brokersLock.Unlock()
	for _, broker := range r.brokers {
		broker.association.Close()
		broker.conn.Close()
	}
}

// getGateway gets or creates a Gateway
func (r *router) getGateway(id string) *gateway.Gateway {
	// We're going to be optimistic and guess that the gateway is already active
	r.gatewaysLock.RLock()
	gtw, ok := r.gateways[id]
	r.gatewaysLock.RUnlock()
	if ok {
		return gtw
	}
	// If it doesn't we still have to lock
	r.gatewaysLock.Lock()
	defer r.gatewaysLock.Unlock()

	gtw, ok = r.gateways[id]
	if !ok {
		gtw = gateway.NewGateway(r.Ctx, id)
		ctx := context.Background()
		ctx = ttnctx.OutgoingContextWithID(ctx, id)
		if r.Identity != nil {
			ctx = ttnctx.OutgoingContextWithServiceInfo(ctx, r.Identity.ServiceName, r.Identity.ServiceVersion, r.Identity.NetAddress)
		}
		gtw.MonitorStream = r.Component.Monitor.GatewayClient(
			ctx,
			grpc.PerRPCCredentials(auth.WithTokenFunc("id", func(ctxID string) string {
				if ctxID != id {
					return ""
				}
				return gtw.Token()
			})),
		)
		r.gateways[id] = gtw
	}

	return gtw
}

const downlinkHandlersPerBroker = 64

// getBroker gets or creates a broker association and returns the broker
// the first time it also starts a goroutine that receives downlink from the broker
func (r *router) getBroker(brokerAnnouncement *pb_discovery.Announcement) (*broker, error) {
	// We're going to be optimistic and guess that the broker is already active
	r.brokersLock.RLock()
	brk, ok := r.brokers[brokerAnnouncement.ID]
	r.brokersLock.RUnlock()
	if ok {
		return brk, nil
	}

	// If it doesn't we still have to lock
	r.brokersLock.Lock()
	defer r.brokersLock.Unlock()
	if brk, ok := r.brokers[brokerAnnouncement.ID]; ok {
		return brk, nil
	}

	brk = &broker{
		uplink: make(chan *pb_broker.UplinkMessage),
	}

	// Connect to the server
	var err error
	brk.conn, err = brokerAnnouncement.Dial(r.Pool)
	if err != nil {
		return nil, err
	}

	// Set up the non-streaming client
	brk.client = pb_broker.NewBrokerClient(brk.conn)

	// Set up the streaming client
	config := brokerclient.DefaultClientConfig
	config.BackgroundContext = r.Component.Context
	cli := brokerclient.NewClient(config)
	cli.AddServer(brokerAnnouncement.ID, brk.conn)
	brk.association = cli.NewRouterStreams(r.Identity.ID, "")

	go func() {
		for {
			message := <-brk.uplink
			brk.association.Uplink(message)
		}
	}()

	logger := r.Ctx.WithField("broker", brokerAnnouncement.ID)
	downlinkHandler := func() {
		downlinkStream := brk.association.Downlink()
		refreshDownlinkStream := time.NewTicker(time.Second)
		for {
			select {
			case <-refreshDownlinkStream.C:
				downlinkStream = brk.association.Downlink()
			case message, ok := <-downlinkStream:
				if !ok {
					logger.Error("Downlink Stream errored")
					downlinkStream = nil
					continue
				}
				if err := r.HandleDownlink(message); err != nil {
					logger.WithError(err).Warn("Failed to handle downlink")
				}
			}
		}
	}

	for i := 0; i < downlinkHandlersPerBroker; i++ {
		go downlinkHandler()
	}

	r.brokers[brokerAnnouncement.ID] = brk

	return brk, nil
}
