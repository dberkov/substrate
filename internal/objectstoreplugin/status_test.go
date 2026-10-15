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
	"errors"
	"fmt"
	"net/http"
	"testing"

	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ublaRefusal is what GCS returns when a bucket with legacy ACLs is read
// with a Workload Identity Federation token, as the public gs://gvisor
// bucket is: the storage client's reads go through the XML API, whose
// response body is all the error carries.
func ublaRefusal() *googleapi.Error {
	return &googleapi.Error{
		Code: http.StatusPreconditionFailed,
		Body: `<?xml version='1.0' encoding='UTF-8'?><Error><Code>PreconditionFailed</Code><Message>The operation requires that Uniform Bucket Level Access be enabled.</Message><Details>The type of authentication token used for this request requires that Uniform Bucket Level Access be enabled.</Details></Error>`,
	}
}

// ublaRefusalJSON is the same refusal from the JSON API, with the
// explanation in a structured error item.
func ublaRefusalJSON() *googleapi.Error {
	const msg = "The type of authentication token used for this request requires that Uniform Bucket Level Access be enabled."
	return &googleapi.Error{
		Code:    http.StatusPreconditionFailed,
		Message: msg,
		Errors:  []googleapi.ErrorItem{{Reason: "conditionNotMet", Message: msg}},
	}
}

func TestToStatusTokenRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want codes.Code
	}{
		{"bucket refuses the token (XML API body)", ublaRefusal(), codes.InvalidArgument},
		{"bucket refuses the token (JSON API items)", ublaRefusalJSON(), codes.InvalidArgument},
		{"wrapped, as the stager reports it", fmt.Errorf("while opening asset gs://gvisor/x: %w", ublaRefusal()), codes.InvalidArgument},
		{"412 for another reason stays internal", &googleapi.Error{Code: http.StatusPreconditionFailed, Errors: []googleapi.ErrorItem{{Reason: "conditionNotMet", Message: "generation mismatch"}}}, codes.Internal},
		{"403 stays internal", &googleapi.Error{Code: http.StatusForbidden, Message: "forbidden"}, codes.Internal},
		{"plain error stays internal", errors.New("boom"), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := status.Code(toStatus(tc.err)); got != tc.want {
				t.Errorf("toStatus(%v) code = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
