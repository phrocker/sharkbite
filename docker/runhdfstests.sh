#!/bin/bash
HOSTNAME=`hostname`
python3.7 hdfswrite.py -f /blahblah6.txt -n hdfs://${HOSTNAME}:9000 -p 9000 -s /pysharkbite.cpython-37m-x86_64-linux-gnu.so
python3.7 hdfslist.py -f / -n hdfs://${HOSTNAME}:9000 -p 9000 -s /pysharkbite.cpython-37m-x86_64-linux-gnu.so
python3.7 hdfsremove.py -f /blahblah6.txt -n hdfs://${HOSTNAME}:9000 -p 9000 -s /pysharkbite.cpython-37m-x86_64-linux-gnu.so
