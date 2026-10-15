// Copyright 2026 Google LLC
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

package objectstoreplugin

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/agent-substrate/substrate/internal/objectstore"
	"github.com/agent-substrate/substrate/pkg/objectstorage"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// toStatus maps a plugin error to a gRPC status, keeping NotFound
// distinguishable so callers can probe for a snapshot, and a bucket that
// refuses the plugin's token distinguishable so atelet can read a public
// asset another way.
func toStatus(err error) error {
	switch {
	case errors.Is(err, objectstorage.ErrObjectNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, objectstore.ErrEmptySource):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case refusesToken(err):
		return status.Errorf(codes.InvalidArgument, "the bucket does not accept the plugin's credentials (it requires uniform bucket-level access for this kind of token), so the plugin cannot serve it: %v", err)
	}
	return status.Error(codes.Internal, err.Error())
}

// refusesToken reports whether GCS refused the plugin's token for the bucket
// rather than the request itself. A bucket with legacy ACLs, uniform
// bucket-level access disabled, rejects a Workload Identity Federation token
// with 412 Precondition Failed, whatever the object's own ACL says. Public
// buckets such as gs://gvisor are read that way, so this is reported as a
// request the plugin cannot serve, InvalidArgument, after which atelet reads
// a gs:// asset anonymously.
//
// The explanation sits in different places depending on the API that
// answered: the JSON API's error items, or the XML API's response body,
// which the storage client keeps verbatim for reads.
func refusesToken(err error) bool {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusPreconditionFailed {
		return false
	}
	const marker = "Uniform Bucket Level Access"
	for _, item := range gerr.Errors {
		if strings.Contains(item.Message, marker) {
			return true
		}
	}
	return strings.Contains(gerr.Message, marker) || strings.Contains(gerr.Body, marker)
}
