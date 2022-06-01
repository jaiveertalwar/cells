/*
 * Copyright (c) 2019-2022. Abstrium SAS <team (at) pydio.com>
 * This file is part of Pydio Cells.
 *
 * Pydio Cells is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio Cells is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio Cells.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */

package manager

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/bep/debounce"
	"golang.org/x/sync/errgroup"

	"github.com/pydio/cells/v4/common"
	"github.com/pydio/cells/v4/common/broker"
	"github.com/pydio/cells/v4/common/config"
	pb "github.com/pydio/cells/v4/common/proto/registry"
	"github.com/pydio/cells/v4/common/registry"
	"github.com/pydio/cells/v4/common/registry/util"
	"github.com/pydio/cells/v4/common/runtime"
	"github.com/pydio/cells/v4/common/server"
	servercontext "github.com/pydio/cells/v4/common/server/context"
	"github.com/pydio/cells/v4/common/service"
	servicecontext "github.com/pydio/cells/v4/common/service/context"
	"github.com/pydio/cells/v4/common/utils/configx"
)

const (
	CommandStart   = "start"
	CommandStop    = "stop"
	CommandRestart = "restart"
)

type Manager interface {
	Init(ctx context.Context) error
	ServeAll(...server.ServeOption)
	StopAll()
	SetServeOptions(...server.ServeOption)
	WatchServicesConfigs()
	WatchBroker(ctx context.Context, br broker.Broker) error
}

type manager struct {
	ns         string
	srcUrl     string
	reg        registry.Registry
	root       registry.Item
	rootIsFork bool

	serveOptions []server.ServeOption

	servers  map[string]server.Server
	services map[string]service.Service
}

func NewManager(reg registry.Registry, srcUrl string, namespace string) Manager {
	m := &manager{
		ns:       namespace,
		srcUrl:   srcUrl,
		reg:      reg,
		servers:  make(map[string]server.Server),
		services: make(map[string]service.Service),
	}
	// Detect a parent root
	var current, parent registry.Item
	if ii, er := reg.List(registry.WithType(pb.ItemType_NODE)); er == nil && len(ii) > 0 {
		for _, root := range ii {
			rPID := root.Metadata()[runtime.NodeMetaPID]
			if rPID == strconv.Itoa(os.Getppid()) {
				parent = root
			} else if rPID == strconv.Itoa(os.Getpid()) {
				current = root
			}
		}
	}
	if current != nil {
		m.root = current
	} else {
		node := util.CreateNode()
		if er := reg.Register(registry.Item(node)); er == nil {
			m.root = node
			if parent != nil {
				m.rootIsFork = true
				_, _ = reg.RegisterEdge(parent.ID(), m.root.ID(), "Fork", map[string]string{})
			}
		}
	}
	return m
}

func (m *manager) Init(ctx context.Context) error {

	srcReg, err := registry.OpenRegistry(ctx, m.srcUrl)
	if err != nil {
		return err
	}

	ctx = servercontext.WithRegistry(ctx, m.reg)
	ctx = servicecontext.WithRegistry(ctx, srcReg)
	runtime.Init(ctx, m.ns)

	services, err := srcReg.List(registry.WithType(pb.ItemType_SERVICE))
	if err != nil {
		return err
	}

	byScheme := map[string]server.Server{}

	for _, ss := range services {
		var s service.Service
		if !ss.As(&s) {
			continue
		}
		opts := s.Options()
		mustFork := opts.Fork && !runtime.IsFork()

		// Replace service context with target registry
		opts.Context = servicecontext.WithRegistry(opts.Context, m.reg)

		if !runtime.IsRequired(s.Name(), opts.Tags...) && !opts.ForceRegister {
			continue
		}

		if mustFork && !opts.AutoStart {
			continue
		}

		scheme := s.ServerScheme()
		if sr, o := byScheme[scheme]; o {
			opts.Server = sr
		} else if srv, er := server.OpenServer(opts.Context, scheme); er == nil {
			byScheme[scheme] = srv
			opts.Server = srv
		} else {
			return er
		}

		if mustFork {
			continue // Do not register here
		}

		if er := m.reg.Register(s, registry.WithEdgeTo(m.root.ID(), "Node", map[string]string{})); er != nil {
			return er
		}

		m.services[s.ID()] = s

	}

	if m.root != nil {
		for _, sr := range byScheme {
			m.servers[sr.ID()] = sr // Keep a ref to the actual object
			_, _ = m.reg.RegisterEdge(m.root.ID(), sr.ID(), "Node", map[string]string{})
		}
	}

	return nil

}

func (m *manager) SetServeOptions(oo ...server.ServeOption) {
	m.serveOptions = oo
}

func (m *manager) ServeAll(oo ...server.ServeOption) {
	m.serveOptions = oo
	opt := &server.ServeOptions{}
	for _, o := range oo {
		o(opt)
	}
	eg := &errgroup.Group{}
	ss := m.serversWithStatus(registry.StatusStopped)
	for _, srv := range ss {
		func(srv server.Server) {
			eg.Go(func() error {
				return m.startServer(srv, oo...)
			})
		}(srv)
	}
	go func() {
		if err := eg.Wait(); err != nil && opt.ErrorCallback != nil {
			opt.ErrorCallback(err)
		}
	}()
}

func (m *manager) StopAll() {
	eg := &errgroup.Group{}
	for _, srv := range m.serversWithStatus(registry.StatusReady) {
		func(sr server.Server) {
			eg.Go(func() error {
				return m.stopServer(sr, registry.WithDeregisterFull())
			})
		}(srv)
	}
	if er := eg.Wait(); er != nil {
		fmt.Println("error while stopping servers", er)
	}
	_ = m.reg.Deregister(m.root, registry.WithRegisterFailFast())
}

func (m *manager) startServer(srv server.Server, oo ...server.ServeOption) error {
	opts := append(oo)
	for _, svc := range m.services {
		if svc.Options().Server == srv {
			if svc.Options().Unique && m.regRunningService(svc.Name()) {
				// There is already a running service here. Do not start now, watch registry and postpone start
				fmt.Printf("There is already a running instance of %s. Do not start now, watch registry and postpone start\n", svc.Name())
				go m.WatchUniqueNeedsStart(svc)
				continue
			}
			opts = append(opts, m.serviceServeOptions(svc)...)
		}
	}
	return srv.Serve(opts...)
}

func (m *manager) stopServer(srv server.Server, oo ...registry.RegisterOption) error {
	// Stop all running services on this server
	eg := &errgroup.Group{}
	for _, svc := range m.servicesRunningOn(srv) {
		func(sv service.Service) {
			eg.Go(func() error {
				return m.stopService(sv, oo...)
			})
		}(svc)
	}
	if er := eg.Wait(); er != nil {
		return er
	}
	// Stop server now
	return srv.Stop(oo...)
}

func (m *manager) startService(svc service.Service) error {
	// Look up for corresponding server
	srv := svc.Options().Server
	serveOptions := append(m.serveOptions, m.serviceServeOptions(svc)...)

	if srv.Is(registry.StatusStopped) {

		fmt.Println("Server is not running, starting " + srv.ID() + " now")
		return srv.Serve(serveOptions...)

	} else if srv.NeedsRestart() {

		fmt.Println("Server needs a restart to append a new service")
		for _, sv := range m.servicesRunningOn(srv) {
			serveOptions = append(serveOptions, m.serviceServeOptions(sv)...)
		}
		if er := m.stopServer(srv); er != nil {
			return er
		}
		return srv.Serve(serveOptions...)

	} else {

		fmt.Println("Starting service")
		if er := svc.Start(); er != nil {
			return er
		}
		if er := svc.OnServe(); er != nil {
			return er
		}

	}

	return nil
}

func (m *manager) stopService(svc service.Service, oo ...registry.RegisterOption) error {
	return svc.Stop(oo...)
}

func (m *manager) serviceServeOptions(svc service.Service) []server.ServeOption {
	return []server.ServeOption{
		server.WithBeforeServe(svc.Start),
		server.WithAfterServe(svc.OnServe),
	}
}

func (m *manager) serversWithStatus(status registry.Status) (ss []server.Server) {
	for _, srv := range m.servers {
		if srv.Is(status) {
			ss = append(ss, srv)
		}
	}
	return
}

func (m *manager) servicesRunningOn(server server.Server) (ss []service.Service) {
	for _, svc := range m.services {
		if svc.Server() == server && svc.Is(registry.StatusReady) {
			ss = append(ss, svc)
		}
	}
	return
}

func (m *manager) WatchServicesConfigs() {
	ch, err := config.WatchMap(configx.WithPath("services"))
	if err != nil {
		return
	}
	for kv := range ch {
		ss, err := m.reg.List(registry.WithName(kv.Key))
		if err != nil || len(ss) == 0 {
			continue
		}
		var svc service.Service
		if ss[0].As(&svc) && svc.Options().AutoRestart {
			if er := m.stopService(svc); er == nil {
				_ = m.startService(svc)
			}
		}
	}
}

func (m *manager) WatchBroker(ctx context.Context, br broker.Broker) error {
	_, er := br.Subscribe(ctx, common.TopicRegistryCommand, func(message broker.Message) error {
		hh, _ := message.RawData()
		cmd := hh["command"]
		itemName := hh["itemName"]
		s, err := m.reg.Get(itemName, registry.WithType(pb.ItemType_SERVER), registry.WithType(pb.ItemType_SERVICE))
		if err != nil {
			if err == os.ErrNotExist {
				return nil
			}
			return err
		}

		var svc service.Service
		var srv server.Server
		var rsrc registry.Service
		var rsrv registry.Server
		if s.As(&svc) || s.As(&srv) {
			// In-memory object found
		} else if s.As(&rsrc) {
			if mem, ok := m.services[s.ID()]; ok {
				svc = mem
			}
		} else if s.As(&rsrv) {
			if mem, ok := m.servers[s.ID()]; ok {
				srv = mem
			}
		}
		if svc == nil && srv == nil {
			return nil
		}
		if svc != nil {
			// Service Commands
			switch cmd {
			case CommandStart:
				return m.startService(svc)
			case CommandStop:
				return m.stopService(svc)
			case CommandRestart:
				if er := m.stopService(svc); er != nil {
					return er
				}
				return m.startService(svc)
			default:
				return fmt.Errorf("unsupported command %s", cmd)
			}
		} else if srv == nil {
			// Server Commands
			switch cmd {
			case CommandStart:
				return m.startServer(srv, m.serveOptions...)
			case CommandStop:
				return m.stopServer(srv)
			case CommandRestart:
				if er := m.stopServer(srv); er != nil {
					return er
				}
				return m.startServer(srv, m.serveOptions...)
			default:
				return fmt.Errorf("unsupported command %s", cmd)
			}
		}
		return nil
	})
	if er != nil {
		fmt.Println("Manager cannot watch broker: ", er)
	}
	return er
}

func (m *manager) regRunningService(name string) bool {
	ll, _ := m.reg.List(registry.WithType(pb.ItemType_SERVICE), registry.WithName(name))
	for _, l := range ll {
		if l.Metadata()[registry.MetaStatusKey] != string(registry.StatusStopped) {
			return true
		}
	}
	return false
}

func (m *manager) WatchUniqueNeedsStart(svc service.Service) {
	db := debounce.New(5 * time.Second)
	w, _ := m.reg.Watch(registry.WithType(pb.ItemType_SERVICE), registry.WithName(svc.Name()), registry.WithAction(pb.ActionType_ANY))
	for {
		_, er := w.Next()
		if er != nil {
			break
		}
		fmt.Println("Event received for service", svc.Name())
		db(func() {
			if !m.regRunningService(svc.Name()) {
				fmt.Println("Starting unique service", svc.Name())
				if er := m.startService(svc); er != nil {
					fmt.Println("Error while starting unique service", svc.Name(), er.Error())
				} else {
					w.Stop()
				}
			}
		})
	}
}