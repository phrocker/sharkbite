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
    table = "blahblahd"

from sharkbite import *

try:
    connector = AccumuloConnector(args.instance,args.zookeepers,args.username,password)

    table_operations = connector.tableOps(table)

    # print tables

    for table in connector.tableInfo().list_tables():
        print(table)

    if not table_operations.exists(False):
        print ("Creating table " + table)
        table_operations.create(False)  
    else:
        print (table + " already exists, so not creating it")  
    

    print ( connector.securityOps().get_auths("root") )
    print ( connector.securityOps().get_auths("root").get_authorizations() )
    for auth in connector.securityOps().get_auths("root").get_authorizations():
        print(auth)

    
    auths = Authorizations()
    
    scanner = table_operations.createScanner(auths, 2)
    
    startKey = Key()
    
    endKey = Key()
    
    range = Range(startKey,True,endKey,False)
    
    scanner.addRange( range )
    
    resultset = scanner.getResultSet()
    
    for keyvalue in resultset:
        key = keyvalue.getKey()
        value = keyvalue.getValue()
        v = value.get()
        print("value is ",v)
    
    
    """ delete your table if user did not create temp """
    
except RuntimeError as e:
     traceback.print_exc()
     print("Oops, error caused: " + str(e))
