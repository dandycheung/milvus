// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package importv2

import (
	"sort"
	"sync"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v2/util/conc"
)

type Scheduler interface {
	Start()
	Slots() int64
	Close()
}

type scheduler struct {
	manager TaskManager

	closeOnce sync.Once
	closeChan chan struct{}
}

func NewScheduler(manager TaskManager) Scheduler {
	return &scheduler{
		manager:   manager,
		closeChan: make(chan struct{}),
	}
}

func (s *scheduler) Start() {
	log.Info("start import scheduler")

	var (
		exeTicker = time.NewTicker(1 * time.Second)
		logTicker = time.NewTicker(10 * time.Minute)
	)
	defer exeTicker.Stop()
	defer logTicker.Stop()

	for {
		select {
		case <-s.closeChan:
			log.Info("import scheduler exited")
			return
		case <-exeTicker.C:
			s.scheduleTasks()
		case <-logTicker.C:
			LogStats(s.manager)
		}
	}
}

func (s *scheduler) scheduleTasks() {
	tasks := s.manager.GetBy(WithStates(datapb.ImportTaskStateV2_Pending))
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].GetTaskID() < tasks[j].GetTaskID()
	})

	if len(tasks) == 0 {
		return
	}

	taskIDs := lo.Map(tasks, func(t Task, _ int) int64 {
		return t.GetTaskID()
	})
	log.Info("processing tasks...", zap.Int64s("taskIDs", taskIDs))

	futures := make(map[int64][]*conc.Future[any])
	for _, task := range tasks {
		fs := task.Execute()
		futures[task.GetTaskID()] = fs
	}

	for taskID, fs := range futures {
		err := conc.AwaitAll(fs...)
		if err != nil {
			continue
		}
		s.manager.Update(taskID, UpdateState(datapb.ImportTaskStateV2_Completed))
		log.Info("preimport/import done", zap.Int64("taskID", taskID))
	}

	log.Info("all tasks completed", zap.Int64s("taskIDs", taskIDs))
}

// Slots returns the used slots for import
func (s *scheduler) Slots() int64 {
	tasks := s.manager.GetBy(WithStates(datapb.ImportTaskStateV2_Pending, datapb.ImportTaskStateV2_InProgress))
	used := lo.SumBy(tasks, func(t Task) int64 {
		return t.GetSlots()
	})
	return used
}

func (s *scheduler) Close() {
	s.closeOnce.Do(func() {
		close(s.closeChan)
	})
}

func tryFreeFutures(futures map[int64][]*conc.Future[any]) {
	for k, fs := range futures {
		fs = lo.Filter(fs, func(f *conc.Future[any], _ int) bool {
			if f.Done() {
				_, err := f.Await()
				return err != nil
			}
			return true
		})
		futures[k] = fs
	}
}
