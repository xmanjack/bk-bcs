/*
 * Tencent is pleased to support the open source community by making Blueking Container Service available.
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package app

import (
	rd "bk-bcs/bcs-common/common/RegisterDiscover"
	"bk-bcs/bcs-common/common/blog"
	"bk-bcs/bcs-common/common/metric"
	"bk-bcs/bcs-common/common/static"
	commtype "bk-bcs/bcs-common/common/types"
	"bk-bcs/bcs-common/common/version"
	"bk-bcs/bcs-mesos/bcs-mesos-watch/cluster"
	"bk-bcs/bcs-mesos/bcs-mesos-watch/cluster/mesos"
	"bk-bcs/bcs-mesos/bcs-mesos-watch/servermetric"
	"bk-bcs/bcs-mesos/bcs-mesos-watch/storage"
	"bk-bcs/bcs-mesos/bcs-mesos-watch/types"
	"encoding/json"
	"fmt"
	"golang.org/x/net/context"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

func RunMetric(cfg *types.CmdConfig) {

	blog.Infof("run metric: port(%d)", cfg.MetricPort)

	conf := metric.Config{
		RunMode:     metric.Master_Slave_Mode,
		ModuleName:  commtype.BCS_MODULE_MESOSDATAWATCH,
		MetricPort:  cfg.MetricPort,
		IP:          cfg.Address,
		ClusterID:   cfg.ClusterID,
		SvrCaFile:   cfg.ServerCAFile,
		SvrCertFile: cfg.ServerCertFile,
		SvrKeyFile:  cfg.ServerKeyFile,
		SvrKeyPwd:   static.ServerCertPwd,
	}

	healthFunc := func() metric.HealthMeta {
		ok, msg := servermetric.IsHealthy()
		role := servermetric.GetRole()
		return metric.HealthMeta{
			CurrentRole: role,
			IsHealthy:   ok,
			Message:     msg,
		}
	}

	if err := metric.NewMetricController(
		conf,
		healthFunc); nil != err {
		blog.Errorf("run metric fail: %s", err.Error())
	}

	blog.Infof("run metric ok")
}

//Run running watch
func Run(cfg *types.CmdConfig) error {

	if cfg.ClusterID == "" {
		blog.Error("datawatcher cluster unknown")
		return fmt.Errorf("datawatcher cluster unknown")
	}
	blog.Info("datawatcher run for cluster %s", cfg.ClusterID)

	//create root context for exit
	rootCxt, rootCancel := context.WithCancel(context.Background())
	interupt := make(chan os.Signal, 10)
	signal.Notify(interupt, syscall.SIGINT, syscall.SIGKILL, syscall.SIGTERM)
	signalCxt, _ := context.WithCancel(rootCxt)
	go handleSysSignal(interupt, signalCxt, rootCancel)

	RunMetric(cfg)

	//create storage
	ccStorage, ccErr := storage.NewCCStorage(cfg)
	if ccErr != nil {
		blog.Error("Create CCStorage Err: %s", ccErr.Error())
		return ccErr
	}
	var DCHosts []string
	ccStorage.SetDCAddress(DCHosts)
	servermetric.SetDCStatus(false)

	ccCxt, _ := context.WithCancel(rootCxt)
	go RefreshDCHost(cfg, ccCxt, ccStorage)
	time.Sleep(2 * time.Second)
	for {
		if ccStorage.GetDCAddress() == "" {
			blog.Warn("storage address is empty, mesos datawatcher cannot run")
			time.Sleep(2 * time.Second)
		} else {
			break
		}
	}

	ccStorage.Run(ccCxt)

	rdCxt, _ := context.WithCancel(rootCxt)
	blog.Info("after storage created, to run server...")
	retry, rdErr := runServer(cfg, rdCxt, ccStorage)
	for retry == true {
		if rdErr != nil {
			blog.Error("run server err: %s", rdErr.Error())
		}
		time.Sleep(3 * time.Second)
		blog.Info("retry run server...")
		retry, rdErr = runServer(cfg, rdCxt, ccStorage)
	}
	if rdErr != nil {
		blog.Error("run server err: %s", rdErr.Error())
	}

	blog.Info("to cancel root after runServer returned")
	rootCancel()
	return rdErr
}

func handleSysSignal(signalChan <-chan os.Signal, exitCxt context.Context, cancel context.CancelFunc) {
	select {
	case s := <-signalChan:
		blog.V(3).Infof("watch Get singal %s, exit!", s.String())
		cancel()
		time.Sleep(2 * time.Second)
		return
	case <-exitCxt.Done():
		blog.V(3).Infof("Signal Handler asked to exit")
		return
	}
}

func runServer(cfg *types.CmdConfig, rdCxt context.Context, storage storage.Storage) (bool, error) {

	servermetric.SetClusterStatus(false, "begin run server")
	servermetric.SetRole(metric.SlaveRole)

	regDiscv := rd.NewRegDiscoverEx(cfg.RegDiscvSvr, time.Second*10)
	if regDiscv == nil {
		servermetric.SetClusterStatus(false, "register error")
		return false, fmt.Errorf("NewRegDiscover(%s) return nil", cfg.RegDiscvSvr)
	}
	blog.Info("NewRegDiscover(%s) succ", cfg.RegDiscvSvr)

	err := regDiscv.Start()
	if err != nil {
		blog.Error("regDisv start error(%s)", err.Error())
		servermetric.SetClusterStatus(false, "register error:"+err.Error())
		return false, err
	}
	blog.Info("RegDiscover start succ")

	blog.Infof("ApplicationThreadNum: %d, TaskgroupThreadNum: %d, ExportserviceThreadNum: %d",
		cfg.ApplicationThreadNum, cfg.TaskgroupThreadNum, cfg.ExportserviceThreadNum)

	host, err := os.Hostname()
	if err != nil {
		blog.Error("mesoswatcher get hostname err: %s", err.Error())
		host = "UNKOWN"
	}
	var regInfo commtype.MesosDataWatchServInfo
	regInfo.ServerInfo.Cluster = cfg.ClusterID
	regInfo.ServerInfo.IP = cfg.Address
	regInfo.ServerInfo.Port = 0
	regInfo.ServerInfo.MetricPort = cfg.MetricPort
	regInfo.ServerInfo.HostName = host
	regInfo.ServerInfo.Scheme = cfg.ServerSchem
	regInfo.ServerInfo.Pid = os.Getpid()
	regInfo.ServerInfo.Version = version.GetVersion()
	data, err := json.Marshal(regInfo)
	key := commtype.BCS_SERV_BASEPATH + "/" + commtype.BCS_MODULE_MESOSDATAWATCH + "/" + cfg.ClusterID + "/" + cfg.Address
	discvPath := commtype.BCS_SERV_BASEPATH + "/" + commtype.BCS_MODULE_MESOSDATAWATCH + "/" + cfg.ClusterID

	err = regDiscv.RegisterService(key, []byte(data))
	if err != nil {
		blog.Error("RegisterService(%s) error(%s)", key, err.Error())
		servermetric.SetClusterStatus(false, "register error:"+err.Error())
		regDiscv.Stop()
		return true, err
	}
	blog.Info("RegisterService(%s:%s) succ", key, data)

	discvEvent, err := regDiscv.DiscoverService(discvPath)
	if err != nil {
		blog.Error("DiscoverService(%s) error(%s)", discvPath, err.Error())
		servermetric.SetClusterStatus(false, "discove error:"+err.Error())
		regDiscv.Stop()
		return true, err
	}
	blog.Info("DiscoverService(%s) succ", discvPath)

	// init, slave, master
	var clusterCancel context.CancelFunc
	var currCluster cluster.Cluster
	clusterCancel = nil
	currCluster = nil

	appRole := "slave"
	tick := time.NewTicker(60 * time.Second)

	for {
		select {
		case <-rdCxt.Done():
			blog.V(3).Infof("runServer asked to exit")
			regDiscv.Stop()
			if currCluster != nil {
				currCluster.Stop()
				currCluster = nil
			}
			if clusterCancel != nil {
				clusterCancel()
			}
			return false, nil
		case <-tick.C:
			blog.V(3).Infof("tick: runServer is alive, current goroutine num (%d)", runtime.NumGoroutine())
			if currCluster != nil && currCluster.GetClusterStatus() != "running" {
				blog.V(3).Infof("tick: current cluster status(%s), to rebuild cluster", currCluster.GetClusterStatus())
				servermetric.SetClusterStatus(false, "cluster status not running")
				regDiscv.Stop()
				if currCluster != nil {
					currCluster.Stop()
					currCluster = nil
				}
				if clusterCancel != nil {
					clusterCancel()
				}
				return true, nil
			}
		case event := <-discvEvent:
			blog.Info("get discover event")
			if event.Err != nil {
				blog.Error("get discover event err:%s", event.Err.Error())
				servermetric.SetClusterStatus(false, "get discove error:"+event.Err.Error())
				regDiscv.Stop()
				if currCluster != nil {
					currCluster.Stop()
					currCluster = nil
				}
				if clusterCancel != nil {
					clusterCancel()
				}
				return true, event.Err
			}

			currRole := ""
			for i, server := range event.Server {
				blog.Info("get discover event: server[%d]: %s %s", i, event.Key, server)
				if currRole == "" && i == 0 && server == string(data) {
					currRole = "master"
					servermetric.SetRole(metric.MasterRole)
					servermetric.SetClusterStatus(true, "master run ok")
				}
				if currRole == "" && i != 0 && server == string(data) {
					currRole = "slave"
					servermetric.SetRole(metric.SlaveRole)
					servermetric.SetClusterStatus(true, "slave run ok")
				}
			}
			if currRole == "" {
				blog.Infof("get discover event, server list len(%d), but cannot find myself", len(event.Server))
				regDiscv.Stop()
				if currCluster != nil {
					currCluster.Stop()
					currCluster = nil
				}
				if clusterCancel != nil {
					clusterCancel()
				}
				servermetric.SetClusterStatus(false, "role error")
				return true, fmt.Errorf("currRole is nil")
			}

			blog.Info("get discover event, curr role: %s", currRole)

			if currRole != appRole {
				blog.Info("role changed: from %s to %s", appRole, currRole)
				appRole = currRole
				if appRole == "master" {
					blog.Info("become to master: to new and run cluster...")
					cluster := mesos.NewMesosCluster(cfg, storage)
					if cluster == nil {
						blog.Error("Create Cluster Error.")
						regDiscv.Stop()
						servermetric.SetClusterStatus(false, "master create cluster error")
						return true, fmt.Errorf("cluster create failed")
					}
					currCluster = cluster
					clusterCxt, cancel := context.WithCancel(rdCxt)
					clusterCancel = cancel
					go cluster.Run(clusterCxt)
				} else {
					blog.V(3).Infof("become to slave: to cancel cluster...")
					if currCluster != nil {
						currCluster.Stop()
						currCluster = nil
					}
					if clusterCancel != nil {
						clusterCancel()
						clusterCancel = nil
					}
				}
			} // end role change
		} // end select
	} // end for

}

func RefreshDCHost(cfg *types.CmdConfig, rfCxt context.Context, storage storage.Storage) {
	blog.Info("mesos data watcher to refresh DCHost ...")
	// register service
	regDiscv := rd.NewRegDiscoverEx(cfg.RegDiscvSvr, time.Second*10)
	if regDiscv == nil {
		blog.Error("NewRegDiscover(%s) return nil", cfg.RegDiscvSvr)
		return
	} 
	blog.Info("NewRegDiscover(%s) succ", cfg.RegDiscvSvr)

	err := regDiscv.Start()
	if err != nil {
		blog.Error("regDiscv start error(%s)", err.Error())
		return
	} 
	blog.Info("RegDiscover start succ")
	
	defer regDiscv.Stop()

	discvPath := commtype.BCS_SERV_BASEPATH + "/" + commtype.BCS_MODULE_STORAGE
	discvEvent, err := regDiscv.DiscoverService(discvPath)
	if err != nil {
		blog.Error("DiscoverService(%s) error(%s)", discvPath, err.Error())
		return
	}
	blog.Info("DiscoverService(%s) succ", discvPath)

	tick := time.NewTicker(120 * time.Second)
	for {
		select {
		case <-tick.C:
			blog.Info("refresh DCHost is running")
			continue
		case <-rfCxt.Done():
			blog.V(3).Infof("refresh DCHost asked to exit")
			return
		case event := <-discvEvent:
			blog.Info("refresh DCHost get discover event")
			if event.Err != nil {
				blog.Error("DCHost discover err:%s", event.Err.Error())
				continue
			}
			blog.Infof("get DCHost node num(%d)", len(event.Server))
			var DCHost string
			var DCHosts []string
			for i, server := range event.Server {
				blog.Infof("get DCHost: server[%d]: %s", i, server)
				var serverInfo commtype.BcsStorageInfo
				if err = json.Unmarshal([]byte(server), &serverInfo); err != nil {
					blog.Errorf("fail to unmarshal DCHost(%s), err:%s", string(server), err.Error())
					continue
				}
				DCHost = serverInfo.ServerInfo.Scheme + "://" + serverInfo.ServerInfo.IP + ":" + strconv.Itoa(int(serverInfo.ServerInfo.Port))
				blog.Infof("get DCHost(%s)", DCHost)
				DCHosts = append(DCHosts, DCHost)
			}

			storage.SetDCAddress(DCHosts)
			if len(DCHosts) > 0 {
				servermetric.SetDCStatus(true)
			} else {
				servermetric.SetDCStatus(false)
			}

		} // end select
	} // end for
}

