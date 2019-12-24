#!/bin/sh

DIR="$( cd "$(dirname "$0")" && pwd )"
source $DIR/sample-env.sh

ACCUMULO_LIB_DIR=/smac-bin/lib


do_work() {
  echo "Doing some work"
  # how do you create a user or add auths?
}

echo Setting up sample
do_work