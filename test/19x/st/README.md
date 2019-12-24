# Standalone Mini Accumulo Cluster (SMAC)

## Description

This project is an experiment with the Mini Accumulo Cluster (MAC).  The intent is to make a Standalone executable jar that runs the MAC.  The code lives at https://gitlab.com/mjwall/standalone-mac.  Note, I moved the code to gitlab from github to try out the CI.

## Branches

There is nothing on master, see other branches like https://gitlab.com/mjwall/standalone-mac/tree/1.9.3

## Building

To build this standalone jar, run the following

    mvn clean package
    
In the target directory, you will see a file with a name like `standalone-mac-shaded-192.1.jar`.  That is the 
executable jar, currently at 44M

## Running

After building the jar, run the following from the project top level directory

    java -jar target/standalone-mac-shaded-193.1.jar
  
Your output should be something like the following


    Starting a Mini Accumulo Cluster:
    InstanceName:       smac
    Root user password: secret
    Temp dir is:        /var/folders/2y/n9lzqm2x10lfxqm9n40xvfvw0000gn/T/1473862620200-0
    Zookeeper is:       localhost:2181
    Monitor:            http://localhost:52922

    Starting a shell
    
    Shell - Apache Accumulo Interactive Shell
    - 
    - version: 1.9.3
    - instance name: smac
    - instance id: 02376280-2881-4c17-9091-aa23b8ee1238
    - 
    - type 'help' for a list of available commands
    - 
    root@smac> exit
    
## Config options

TODO: add them all

### tempMiniDir - temp directory used by the SMAC
It will be deleted when you exit.  To make it persist pass in a `-DtempMiniDir=/some/path` option before the `-jar` 
command.  You can then vist that directory and see all the logs.

### startShell - whether to start a shell when the SMAC runs

By default, this is set to true.  When true, an accumulo shell is launched when the SMAC starts up.  The SMAC stays
running until you exit the shell.  If no shell is run, you have to Ctrl-C to stop

### initScript - initialization script

Make a shell script and use -DinitScript to point to it.  This script will be run after the cluster starts up, but
before the shell is invoked (unless you set startShell false).  Use this script to do things like add jars to the 
lib/ext or run some other initialization stuff like creating users or adding auths.

### setJDWP - set Java Debug Wire Protocol

Part of the Mini Accumulo Cluster, setting this to true will start up debug ports for each process so you can 
attach a remote debugger.  To find the port for a process, look at the options it was started with.  First, find the
pid.

    ps -ef | grep '\-Dproc=TabletServer'
    
This will show the pid and usually you can see the jdwp port.  It will be something like 

    -Xrunjdwp:transport=dt_socket,server=y,suspend=n,address=40973
    
That 40973 is your debug port.  Jetty has a nice doc on remote debugging at 
http://www.eclipse.org/jetty/documentation/9.4.x/enable-remote-debugging.html.

If you can see the port from the ps command, you can also run something like

    jcmd <pid> VM.command_line

## Running from maven

You can also run the SMAC from maven with the following

    mvn clean test -Drun
    
The log directory will be `target/mini`

## Running a shell for an already running SMAC

If you already have a running SMAC, run the following to get an Accumulo shell.

    java -cp target/standalone-mac-shaded-193.1-SNAPSHOT.jar com.mjwall.accumulo.commands.SmacAccumuloShell

## Getting into zookeeper

You still need a running SMAC, but you can run the following to get Zookeeper cli.

    java -cp target/standalone-mac-shaded-193.1.jar com.mjwall.accumulo.commands.SmacZookeeperCli
    
## Docker

TODO: add more stuff here

### Compose

Start zookeeper and standalone so that they can be connected from external processes:

This will expose the port for zookeeper from the `docker-compose.yml` file and will
expose the appropriate port from the accumulo node on your local machine so that processes
run external to the compose can connect.  For a client, you'd connect on `localhost:21811` with
the existing config.

```
docker-compose up
```

### Expose ports

NOTE: this doesn't work, just run with --net=host for now

Start a container and find all the ports

````
make run_d
make attach
cat ~/.smac-running-env
BLAH # note <zookeeper_port> and <monitor_port>
ps -efl | grep Dproc=Tablet
BLAH2 with remote debug port <tserver_jdwp_port>
````
Say we want to expose port 21811 for Zookeeper, 40023 for monitor and 38161 for the remote debug port on the tserver.  
Make a new image for the running container

```
docker ps
docker commit <container_id> mjwall:smac-docker-single:debug1
make stop
docker images
```
Now you got a docker image id for the debug1 tag

```
docker run -d -p 21811:<zookeeper_port> -p 40023:<monitor_port> -p 38161:<tserver_jdwp_port> mjwall:smac-docker-single:debug1
```

## Version numbers

Just a quick note about the version numbers.  Unlike semver, this project is using the following annotation

```
ACCUMULO\_NUMBER.SMAC\_VERSION.DOCKER_VERSION
```

The ACCUMULO\_NUMBER is the version of Accumulo with the dots removed.  So if you see 193, that based against Accumulo version 1.9.3.  

The SMAC\_VERSION is a 1 up number to allow for changes in the Standalone MiniAccumuloCluster code.  The artifacts produced by Maven will only have ACCUMULO\_NUMBER.SMAC\_VERSION numbers.

The DOCKER_VERSION allows for changes to the docker build independent of SMAC.  It is also a 1 up number.

## Gcore and jmap

sudo gcore -o /tmp/tserver.core <PID>
sudo /path/to/jdk/bin/jmap \
           -dump:format=b,file=/tmp/worker-jvm.hprof \
           /usr/bin/java /tmp/worker-jvm.core
