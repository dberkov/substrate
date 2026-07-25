#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# This is sourced as part of install-ate.sh. Do not run directly.
#
# Image-streaming (Riptide/GCFS) PoC demo. Prerequisites the operator must set
# up first (see demos/streaming/README.md); this hook does NOT create them:
#   * image streaming enabled on the node pool the workers run on
#     (--enable-image-streaming)
#   * atelet started with --image-streaming-poc=true
#   * registry read (artifactregistry.reader + storage.objectViewer) for the
#     ate-demo-streaming/default workload-identity principal

ATE_DEMOS+=(demo-streaming) # register demo-streaming

demo-streaming_cmdline() {
  case "${1}" in
    --deploy-demo-streaming) demo-streaming_deploy ;;
    --delete-demo-streaming) demo-streaming_delete ;;
    *)
      return 1
      ;;
  esac
  return 0
}

demo-streaming_deploy() {
  log_step "demo-streaming_deploy"
  ensure_crds
  # ko builds the counterheavy server onto the large base from .ko.yaml and
  # resolves the ko:// ref to a pinned digest.
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" demos/streaming/counter-heavy.yaml.tmpl \
    | run_ko apply -f -

  log_step "Waiting for streaming demo to be ready..."
  run_kubectl rollout status deployment/streaming -n ate-demo-streaming --timeout=300s
  run_kubectl wait --for=condition=Ready actortemplate/counter-heavy -n ate-demo-streaming --timeout=600s
}

demo-streaming_delete() {
  log_step "demo-streaming_delete"
  delete_demo_actors ate-demo-streaming counter-heavy
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" demos/streaming/counter-heavy.yaml.tmpl \
    | run_kubectl delete --ignore-not-found -f -
}
