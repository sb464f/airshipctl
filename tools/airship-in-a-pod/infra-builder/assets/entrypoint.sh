#!/bin/bash

# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -ex

# Create the "canary" file, indicating that the container is healthy
mkdir -p /tmp/healthy
touch /tmp/healthy/infra-builder

success=false
function cleanup() {
  if [[ "$success" == "false" ]]; then
    rm /tmp/healthy/infra-builder
  fi
}
trap cleanup EXIT

printf "Waiting 30 seconds for the libvirt and docker services to be ready\n"
sleep 30

ansible-playbook -v /opt/ansible/playbooks/build-infra.yaml \
  -e local_src_dir="$(pwd)"

success=true
/signal_complete infra-builder
