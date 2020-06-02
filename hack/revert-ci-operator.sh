#!/usr/bin/env bash

set -euo pipefail


OLD_LATEST="$(oc --context api.ci get is ci-operator -n ci -o jsonpath={.status.tags[?\(@.tag==\"latest\"\)].items[1].dockerImageReference}|cut -d '@' -f2)"

echo "execute \`oc --context api.ci tag ci/ci-operator@$OLD_LATEST ci/ci-operator:latest\`"
