// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mincauthority

import (
	"context"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

// HostedOwnerVerifier binds minor-compaction authority to the exact hosted
// assignment. VerifyHosted rejects graceful unload, forced unload, lock loss,
// and a later assignment attempt.
type HostedOwnerVerifier struct {
	Host    *tserver.Host
	Fence   tserver.Fence
	Attempt tserver.Attempt
}

func (v HostedOwnerVerifier) Verify(ctx context.Context, fence ingestrouter.Fence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.Host == nil || !v.Attempt.Valid() ||
		fence.ServerGeneration != v.Fence.Server.String() ||
		fence.ManagerGeneration != v.Fence.Manager.String() ||
		fence.Assignment != v.Attempt.Assignment() {
		return ErrStaleOwner
	}
	if err := v.Host.VerifyHosted(v.Fence, v.Attempt); err != nil {
		return fmt.Errorf("%w: %v", ErrStaleOwner, err)
	}
	return nil
}
