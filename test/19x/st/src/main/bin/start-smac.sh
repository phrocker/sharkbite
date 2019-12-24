#!/bin/sh

DIR="$( cd "$(dirname "$0")" && pwd )"
source $DIR/sample-env.sh

# check for ~/.smac-running-env and then check that
# processes are running before doing this.  Then the
# expose ports stuff in the readme should work

SMAC_OPTS="-DtempMiniDir=/smac-tmp"
SMAC_OPTS="${SMAC_OPTS} -DinstanceName=${ACCUMULO_INSTANCE}"
SMAC_OPTS="${SMAC_OPTS} -DrootPassword=${ACCUMULO_ROOT_PASS}"
SMAC_OPTS="${SMAC_OPTS} -DnumTservers=${ACCUMULO_NUM_TSERVERS}"
SMAC_OPTS="${SMAC_OPTS} -DinitScript=${ACCUMULO_INIT_SCRIPT}"
SMAC_OPTS="${SMAC_OPTS} -DsetJDWP=${ACCUMULO_JDWP}"
SMAC_OPTS="${SMAC_OPTS} -DstartShell=${ACCUMULO_START_SHELL}"
SMAC_OPTS="${SMAC_OPTS} -DshutdownPort=${ACCUMULO_SHUTDOWN_PORT}"
SMAC_OPTS="${SMAC_OPTS} -Dlog4jConfig=${LOG4J_CONFIG}"
SMAC_OPTS="${SMAC_OPTS} -DmonitorPort=${ACCUMULO_MONITOR_PORT}"

if [ -n "${TSERV_CLIENTPORT}" ]; then
    SMAC_OPTS="${SMAC_OPTS} -DtserverClientPort=${TSERV_CLIENTPORT}"
fi

if [ -n $EXISTING_ZOOKEEPERS ]; then
    SMAC_OPTS="${SMAC_OPTS} -DexistingZookeepers=${EXISTING_ZOOKEEPERS}"
else
    SMAC_OPTS="${SMAC_OPTS} -DzookeeperPort=${ZOOKEEPER_PORT}"
fi

SMAC_JAR=$(ls /downloads/standalone*.jar | head -1)

java $SMAC_OPTS -jar $SMAC_JAR

