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



"""

This is an Example of using the Python connectors. The example will accept user input
create a table writing arbitrary information to it via the BatchWriter and scanner will put the written data      
     
             
"""

parser = ArgumentParser(description="This is an Apache Accummulo Python connector")

parser.add_argument("-i", "--instance", dest="instance",
                    help="Apache Accumulo Instance Name", required=True)
parser.add_argument("-z", "--zookeepers", dest="zookeepers",
                    help="Comma Separated Zookeeper List", required=True)
parser.add_argument("-u", "--username", dest="username",
                    help="User to access Apache Accumulo", required=True)
parser.add_argument("-p", "--password", dest="password",
                    help="Password to access Apache Accumulo. May also be supplied at the command line")
parser.add_argument("-t", "--table", dest="table",
                    help="Table to create/update")
args = parser.parse_args()

password = args.password
table = args.table

if not password:
    print("Please enter your password")
    password = input()
    
if not table:
    table = "test"

from sharkbite import *


try:
    writer = AccumuloWriter(args.instance,args.zookeepers,args.username,password,table,"")

    for i in range(5000):
        writer.put("q",cf="cf" + str(i),cq="cq")
        writer.put("q",cf="cf2"+ str(i),cq="cq")
    
    writer.close()

    scanner = writer.to_scanner()
    for keyvalue in scanner.get("q"):
        key = keyvalue.getKey()
        value = keyvalue.getValue()
        v = value.get()
        writer.delete(key)
        print("key is " + str(key))

    
except RuntimeError as e:
     traceback.print_exc()
     print("Oops, error caused: " + str(e))
