// Copyright 2017 Authors of Cilium
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

package launch

import (
	"bufio"
	"encoding/json"
	"os"
	"syscall"
	"time"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/launcher"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging"
)

var log = logging.DefaultLogger

const targetName = "cilium-node-monitor"

// NodeMonitor is used to wrap the node executable binary.
type NodeMonitor struct {
	launcher.Launcher

	state *models.MonitorStatus

	PipeLock lock.Mutex
	Pipe     *os.File
}

// GetPid returns the node monitor's pid.
func (m *NodeMonitor) GetPid() int {
	return m.GetProcess().Pid
}

// Run starts the node monitor.
func (nm *NodeMonitor) Run(sockPath string) {
	nm.SetTarget(targetName)
	for {
		os.Remove(sockPath)
		if err := syscall.Mkfifo(sockPath, 0600); err != nil {
			log.WithError(err).Fatalf("Unable to create named pipe %s", sockPath)
			time.Sleep(time.Duration(5) * time.Second)
		}

		pipe, err := os.OpenFile(sockPath, os.O_RDWR, 0600)
		if err != nil {
			log.WithError(err).Fatal("Unable to open named pipe for writing")
			time.Sleep(time.Duration(5) * time.Second)
		}

		nm.Mutex.Lock()
		nm.Pipe = pipe
		nm.Mutex.Unlock()

		nm.Launcher.Run()

		r := bufio.NewReader(nm.GetStdout())
		for nm.GetProcess() != nil {
			l, _ := r.ReadBytes('\n')
			var tmp *models.MonitorStatus
			if err := json.Unmarshal(l, &tmp); err != nil {
				continue
			}
			nm.setState(tmp)
		}

		pipe.Close()
	}
}

// State returns the monitor status.
func (nm *NodeMonitor) State() *models.MonitorStatus {
	nm.Mutex.RLock()
	state := nm.state
	nm.Mutex.RUnlock()
	return state
}

// setState sets the internal state monitor with the given state.
func (nm *NodeMonitor) setState(state *models.MonitorStatus) {
	nm.Mutex.Lock()
	nm.state = state
	nm.Mutex.Unlock()
}
