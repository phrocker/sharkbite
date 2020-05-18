#!/usr/bin/python
# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
from ctypes import cdll
from argparse import ArgumentParser
from ctypes import cdll
import ctypes
import traceback
import time



"""

This is an Example of using the Python connectors. The example will accept user input
create a table writing arbitrary information to it via the BatchWriter and scanner will put the written data      
     
             
"""

parser = ArgumentParser(description="This is an Apache Accummulo Python connector")

parser.add_argument("-f", "--hdfspath", dest="hdfspath", help="Hdfs Path", required=True)
parser.add_argument("-n", "--namenode", dest="nn", help="Namenode hostname", required=True) 

parser.add_argument("-p", "--namenodeport", dest="port", help="Namenode port", required=True) 

parser.add_argument("-s", "--solocation", dest="sharedobject", help="Shared object Location")

args = parser.parse_args()


if not args.sharedobject is None:
    print ("Opening ",args.sharedobject)
    py = cdll.LoadLibrary(args.sharedobject)
    
import pysharkbite 

hdfs = pysharkbite.Hdfs(args.nn,int(args.port))

read = hdfs.read(args.hdfspath)

print ("got ", read.readString())

#assert("hello world" == read.readString())

hdfs.remove(args.hdfspath,False)
