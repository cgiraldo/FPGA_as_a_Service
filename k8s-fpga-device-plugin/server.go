// Portions Copyright 2018-2021 Xilinx Inc.
// FPGA as a Service (k8s_dev@xilinx.com)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	"net"
	"os"
	"path"
	"reflect"
	_ "runtime/debug"
	"strings"
	"time"
)

const (
	resourceNamePrefix = "xilinx.com/fpga"
	serverSockPath     = pluginapi.DevicePluginPath
	AWS_SN             = "F1-Node"
)

// FPGADevicePluginServer implements the Kubernetes device plugin API
type FPGADevicePluginServer struct {
	devType string
	devices map[string]Device
	socket  string
	stop    chan interface{}
	update  chan map[string]Device

	server *grpc.Server
}

type FPGADevicePlugin struct {
	devices    map[string]map[string]Device
	servers    map[string]*FPGADevicePluginServer
	updateChan chan map[string]map[string]Device
}

// NewFPGADevicePlugin returns an initialized FPGADevicePlugin
func NewFPGADevicePlugin() *FPGADevicePlugin {
	log.Debugf("create FPGA device plugin")
	updateChan := make(chan map[string]map[string]Device)
	plugin := FPGADevicePlugin{
		devices:    make(map[string]map[string]Device),
		servers:    make(map[string]*FPGADevicePluginServer),
		updateChan: updateChan,
	}

	go func() {
		for {
			devices, err := GetDevices()
			if err != nil {
				time.Sleep(75 * time.Second)
				devices, err = GetDevices()
				if err != nil {
					log.Errorf("Error to get FPGA devices: %v", err)
					break
				}
			}
			devMap := make(map[string]map[string]Device)
			for _, device := range devices {
				DSAtype := device.shellVer + "-" + device.timestamp
				id := device.DBDF
				if subMap, ok := devMap[DSAtype]; ok {
					subMap = devMap[DSAtype]
					subMap[id] = device
				} else {
					subMap = make(map[string]Device)
					devMap[DSAtype] = subMap
					subMap[id] = device
				}
			}
			updateChan <- devMap
			time.Sleep(5 * time.Second)
		}
		close(updateChan)
	}()

	return &plugin
}

func (m *FPGADevicePlugin) checkDeviceUpdate(n map[string]map[string]Device) {
	added := make(map[string]map[string]Device)
	updated := make(map[string]map[string]Device)
	removed := make(map[string]map[string]Device)

	for oDevType, oDevices := range m.devices {
		if nDevices, ok := n[oDevType]; ok {
			if !reflect.DeepEqual(oDevices, nDevices) {
				updated[oDevType] = nDevices
			}
			delete(n, oDevType)
		} else {
			removed[oDevType] = oDevices
		}
	}
	for nDevType, nDevices := range n {
		added[nDevType] = nDevices
	}

	//create new server for added devices
	for aDevType, aDevices := range added {
		devicePluginServer := m.NewFPGADevicePluginServer(aDevType, aDevices)
		m.devices[aDevType] = aDevices
		m.servers[aDevType] = devicePluginServer
		go func(aDevType string, aDevices map[string]Device, name string) {
			if err := m.servers[aDevType].Serve(name); err != nil {
				log.Println("Could not contact Kubelet, Exit. Did you enable the device plugin feature gate?")
				os.Exit(1)
			}
			m.servers[aDevType].update <- aDevices
		}(aDevType, aDevices, resourceNamePrefix+"-"+aDevType)
	}

	//stop server for removed devices
	for rDevType, rDevices := range removed {
		log.Debugf("Remove device %v", rDevices)
		m.servers[rDevType].Stop()
		delete(m.servers, rDevType)
		delete(m.devices, rDevType)
	}

	//send update for updated devices
	for uDevType, uDevices := range updated {
		m.devices[uDevType] = uDevices
		m.servers[uDevType].update <- uDevices
	}
}

// NewFPGADevicePluginServer returns an initialized FPGADevicePluginServer
func (m *FPGADevicePlugin) NewFPGADevicePluginServer(devType string, devices map[string]Device) *FPGADevicePluginServer {
	return &FPGADevicePluginServer{
		devType: devType,
		devices: devices,
		socket:  path.Join(serverSockPath, devType+"-fpga.sock"),
		stop:    make(chan interface{}),
		update:  make(chan map[string]Device, 1),
	}
}

// waitForServer checks if grpc server is alive
// by making grpc blocking connection to the server socket
func waitForServer(socket string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := grpc.DialContext(ctx, socket, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)
	if conn != nil {
		conn.Close()
	}

	if err != nil {
		fmt.Errorf("Failed dial context at %s", socket)
		return err
	}
	return nil
}

func (m *FPGADevicePluginServer) deviceExists(id string) bool {
	for k, _ := range m.devices {
		if k == id {
			return true
		}
	}
	return false
}

func (m *FPGADevicePluginServer) GetPreferredAllocation(ctx context.Context, rqt *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return new(pluginapi.PreferredAllocationResponse), nil
}

func (m *FPGADevicePluginServer) PreStartContainer(ctx context.Context, rqt *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return nil, fmt.Errorf("PreStartContainer() should not be called")
}

func (m *FPGADevicePluginServer) GetDevicePluginOptions(ctx context.Context, empty *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	fmt.Println("GetDevicePluginOptions: return empty options")
	return new(pluginapi.DevicePluginOptions), nil
}

// Start starts the gRPC server of the device plugin
func (m *FPGADevicePluginServer) Start() error {
	err := m.cleanup()
	if err != nil {
		return err
	}

	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	m.server = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(m.server, m)

	go m.server.Serve(sock)

	// Wait for the server to start
	if err = waitForServer(m.socket, 10*time.Second); err != nil {
		return err
	}

	return nil
}

// Stop stops the gRPC server
func (m *FPGADevicePluginServer) Stop() error {
	if m.server == nil {
		return nil
	}

	m.server.Stop()
	m.server = nil
	close(m.stop)
	close(m.update)

	return m.cleanup()
}

func (m *FPGADevicePluginServer) cleanup() error {
	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (m *FPGADevicePluginServer) Register(kubeletEndpoint, resourceName string) error {
	conn, err := grpc.Dial(kubeletEndpoint, grpc.WithInsecure(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}))

	if err != nil {
		log.Debugf("Cann't connect to kubelet service")
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: resourceName,
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		log.Debugf("Cann't register to kubelet service")
		return err
	}
	return nil
}

func IsContain(items []string, item string) bool {
	AWS_SN := "F1-Node"
	for _, eachItem := range items {
		if eachItem == item && strings.EqualFold(item, AWS_SN) != true {
			return true
		}
	}
	return false
}

func (m *FPGADevicePluginServer) sendDevices(s pluginapi.DevicePlugin_ListAndWatchServer) error {
	resp := new(pluginapi.ListAndWatchResponse)

	test_range := m.devices
	SerialNums := []string{}
	for _, device := range test_range {
		if IsContain(SerialNums, device.SN) && device.SN != "" {
			log.Printf("Same serial number device already exists")
		} else {
			if device.SN == "" {
				log.Printf("Error, Device %v has empty Serial number", device.DBDF)
			} else {
				SerialNums = append(SerialNums, device.SN)
				resp.Devices = append(resp.Devices, &pluginapi.Device{ID: device.DBDF, Health: device.Healthy})
			}
		}
	}
	log.Printf("Check SeialNums arry: %v", SerialNums)
	log.Printf("Sending %d device(s) %v to kubelet", len(resp.Devices), resp.Devices)
	if err := s.Send(resp); err != nil {
		m.Stop()
		log.Debugf("Cannot update device list")
		return err
	}
	return nil
}

// ListAndWatch lists devices and update that list according to the health status
func (m *FPGADevicePluginServer) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	log.Debugf("In ListAndWatch(%s): stream: %v", m.devType, s)
	//debug.PrintStack()
	for m.devices = range m.update {
		if err := m.sendDevices(s); err != nil {
			return err
		}
	}
	return nil
}

// Allocate which return list of devices.
func (m *FPGADevicePluginServer) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	log.Debugf("In Allocate()")
	AWS_SN := "F1-Node"
	response := new(pluginapi.AllocateResponse)
	for _, creq := range req.ContainerRequests {
		log.Debugf("Request IDs: %v", creq.DevicesIDs)

		cres := new(pluginapi.ContainerAllocateResponse)

		// Check same serial number devices, devices with same serail number "F1-node" will be marked as independent devices
		deviceIDs_arry := creq.DevicesIDs
		for id2 := range deviceIDs_arry {
			for _, device := range m.devices {
				if device.SN == m.devices[deviceIDs_arry[id2]].SN && strings.EqualFold(device.SN, AWS_SN) != true && IsContain(deviceIDs_arry, device.DBDF) == false {
					deviceIDs_arry = append(deviceIDs_arry, device.DBDF)
				}
			}
		}
		log.Println("Check final allocate: %v", deviceIDs_arry)

		for _, id := range deviceIDs_arry {
			log.Printf("Receiving request %s", id)
			dev, ok := m.devices[id]
			if !ok {
				return nil, fmt.Errorf("Invalid allocation request with non-existing device %s", id)
			}
			if !m.deviceExists(id) {
				return nil, fmt.Errorf("invalid allocation request: unknown device: %s", id)
			}

			// Before we have mgmt and user pf separated, we add both to the device cgroup.
			// It is still safe with mgmt pf assigned to container since xilinx device driver
			// makes sure flashing DSA(shell) through mgmt pf in container is denied.
			// This is not good. we will change that later, then only the user pf node is
			// required to be assigned to container(device cgroup of the container)
			//
			// When containers are on top of VM, it is possible only user PF is assigned
			// to VM, so the Mgmt is empty. Don't add it to cgroup in that case
			if dev.Nodes.Mgmt != "" {
				cres.Devices = append(cres.Devices, &pluginapi.DeviceSpec{
					HostPath:      dev.Nodes.Mgmt,
					ContainerPath: dev.Nodes.Mgmt,
					Permissions:   "rwm",
				})
				cres.Mounts = append(cres.Mounts, &pluginapi.Mount{
					HostPath:      dev.Nodes.Mgmt,
					ContainerPath: dev.Nodes.Mgmt,
					ReadOnly:      false,
				})
			}
			cres.Devices = append(cres.Devices, &pluginapi.DeviceSpec{
				HostPath:      dev.Nodes.User,
				ContainerPath: dev.Nodes.User,
				Permissions:   "rwm",
			})
			cres.Mounts = append(cres.Mounts, &pluginapi.Mount{
				HostPath:      dev.Nodes.User,
				ContainerPath: dev.Nodes.User,
				ReadOnly:      false,
			})
			// if this device supports qdma, assign the qdma node to pod too
			if dev.Nodes.Qdma != "" {
				cres.Devices = append(cres.Devices, &pluginapi.DeviceSpec{
					HostPath:      dev.Nodes.Qdma,
					ContainerPath: dev.Nodes.Qdma,
					Permissions:   "rwm",
				})
				cres.Mounts = append(cres.Mounts, &pluginapi.Mount{
					HostPath:      dev.Nodes.Qdma,
					ContainerPath: dev.Nodes.Qdma,
					ReadOnly:      false,
				})
			}
		}
		response.ContainerResponses = append(response.ContainerResponses, cres)
	}

	return response, nil
}

// Serve starts the gRPC server and register the device plugin to Kubelet
func (m *FPGADevicePluginServer) Serve(resourceName string) error {
	log.Debugf("In Serve(%s)", m.socket)
	err := m.Start()
	if err != nil {
		log.Errorf("Could not start device plugin: %v", err)
		return err
	}
	log.Infof("Starting to serve on %s", m.socket)

	err = m.Register(pluginapi.KubeletSocket, resourceName)
	if err != nil {
		log.Errorf("Could not register device plugin: %v", err)
		m.Stop()
		return err
	}
	log.Infof("Registered device plugin with Kubelet %s", resourceName)

	return nil
}
