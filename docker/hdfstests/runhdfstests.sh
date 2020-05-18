#!/bin/bash

PORT=8020
SHUTDOWN_PORT=8181
IMAGE_ID=`docker run -d phrocker/hadoop:latest`
CONTAINER_IP=`docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' ${IMAGE_ID}`
HOSTNAME=${CONTAINER_IP}

echo "Created container ${CONTAINER_IP}"
for script in `ls *.py` 
do
	COMMAND="python3 ${script} -f /testfile.txt -n hdfs://${HOSTNAME}:${PORT} -p ${PORT}"
	echo "Running ${COMMAND}"
	$COMMAND
done

telnet ${CONTAINER_IP} ${SHUTDOWN_PORT}
