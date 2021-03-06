// Copyright © 2017 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package broker

import (
	"testing"

	pb "github.com/TheThingsNetwork/api/broker"
	"github.com/TheThingsNetwork/api/monitor/monitorclient"
	"github.com/TheThingsNetwork/ttn/core/component"
	"github.com/TheThingsNetwork/ttn/core/types"
	. "github.com/TheThingsNetwork/ttn/utils/testing"
	. "github.com/smartystreets/assertions"
)

func TestDownlink(t *testing.T) {
	a := New(t)

	appEUI := types.AppEUI{0, 1, 2, 3, 4, 5, 6, 7}
	devEUI := types.DevEUI{0, 1, 2, 3, 4, 5, 6, 7}

	dlch := make(chan *pb.DownlinkMessage, 2)
	logger := GetLogger(t, "TestDownlink")
	b := &broker{
		Component: &component.Component{
			Ctx:     logger,
			Monitor: monitorclient.NewMonitorClient(),
		},
		ns: &mockNetworkServer{},
		routers: map[string]*router{
			"routerID": &router{downlinkConns: 1, downlink: dlch},
		},
	}
	b.InitStatus()

	err := b.HandleDownlink(&pb.DownlinkMessage{
		DevEUI: devEUI,
		AppEUI: appEUI,
		DownlinkOption: &pb.DownlinkOption{
			Identifier: "fakeID",
		},
	})
	a.So(err, ShouldNotBeNil)

	err = b.HandleDownlink(&pb.DownlinkMessage{
		DevEUI: devEUI,
		AppEUI: appEUI,
		DownlinkOption: &pb.DownlinkOption{
			Identifier: "nonExistentRouterID:scheduleID",
		},
	})
	a.So(err, ShouldNotBeNil)

	err = b.HandleDownlink(&pb.DownlinkMessage{
		DevEUI: devEUI,
		AppEUI: appEUI,
		DownlinkOption: &pb.DownlinkOption{
			Identifier: "routerID:scheduleID",
		},
	})
	a.So(err, ShouldBeNil)
	a.So(len(dlch), ShouldEqual, 1)
}
