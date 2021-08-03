#!/bin/bash
set -euo pipefail
cargo build && go build testclient/main.go && ./main $@
