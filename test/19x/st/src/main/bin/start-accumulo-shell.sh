#!/bin/sh

SMAC_JAR=$(ls /downloads/standalone*.jar | head -1)

java -cp $SMAC_JAR com.mjwall.accumulo.commands.SmacAccumuloShell