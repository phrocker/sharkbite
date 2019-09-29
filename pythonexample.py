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
import os
import traceback
import sys
import time

parser = ArgumentParser()
parser.add_argument("-d", "--dll", dest="dll_file",
                    help="DLL filename", metavar="FILE")
parser.add_argument("-i", "--instance", dest="instance",
                    help="DLL filename")
parser.add_argument("-z", "--zookeepers", dest="zookeepers",
                    help="DLL filename")
parser.add_argument("-u", "--username", dest="username",
                    help="DLL filename")
parser.add_argument("-p", "--password", dest="password",
                    help="DLL filename")
args = parser.parse_args()

""" dll_file is the path to the shared object """

import pysharkbite

conf = pysharkbite.Configuration()

conf.set ("FILE_SYSTEM_ROOT", "/accumulo");

zk = pysharkbite.ZookeeperInstance(args.instance, args.zookeepers, 1000, conf)

user = pysharkbite.AuthInfo("root", "secret", zk.getInstanceId()) 

try:
    connector = pysharkbite.AccumuloConnector(user, zk)

    tableOperations = connector.tableOps("blahblahd")

    print(str(tableOperations.exists(False)))
    
    tableOperations.create(False)
    
    auths = pysharkbite.Authorizations()
    
    writer = tableOperations.createWriter(auths, 10)
    
    mutation = pysharkbite.Mutation("row2");
    
    mutation.put("cf","cq","cv",1569786960)
    
    writer.addMutation( mutation )
    
    writer.close()
    
    time.sleep(2)
    
    auths.addAuthorization("cv")
    
    scanner = tableOperations.createScanner(auths, 2)
    
    startKey = pysharkbite.Key()
    
    endKey = pysharkbite.Key()
    
    startKey.setRow("row")
    
    endKey.setRow("row3")
    
    range = pysharkbite.Range(startKey,True,endKey,False)
    
    scanner.addRange( range )
    
    resultset = scanner.getResultSet()
    
    for keyvalue in resultset:
        key = keyvalue.getKey()
        print(key.getRow() + ":" + key.getColumnFamily() + ":" +  key.getColumnQualifier() + " [" + key.getColumnVisibility() + "]")
    
    print(str(tableOperations.exists(False)))
    
    """ tableOperations.remove()"""
    
except RuntimeError as e:
     traceback.print_exc()
     print("Oops, error caused: " + str(e))
