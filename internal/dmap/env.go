// Copyright 2018-2022 Burak Sezer
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

package dmap

import (
	"time"

	"github.com/buraksezer/olric/internal/cluster/partitions"
)

type env struct {
	putConfig *putConfig
	hkey      uint64
	timestamp int64
	dmap      string
	key       string
	value     []byte
	timeout   time.Duration
	kind      partitions.Kind
	fragment  *fragment
}

func newEnv() *env {
	return &env{
		putConfig: &putConfig{},
		timestamp: time.Now().UnixNano(),
		kind:      partitions.PRIMARY,
	}
}
