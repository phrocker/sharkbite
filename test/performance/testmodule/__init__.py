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

class TestRunner(object):


	def __init__(self):
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
		parser.add_argument("-s", "--solocation", dest="shardobject",
		                    help="Shared object Location")
		args = parser.parse_args()
		
		instance = args.instance
		zookeepers = args.zookeepers
		password = args.password
		self._table = args.table
		dll = args.shardobject
		
		print ("Opening ",dll)
		py = cdll.LoadLibrary(dll)
		
		import sharkbite
		
		self._conf = sharkbite.Configuration()
		
		self._conf.set ("FILE_SYSTEM_ROOT", "/accumulo");
		
		self._zk = sharkbite.ZookeeperInstance(args.instance, args.zookeepers, 2000, self._conf)
		
		self._user = sharkbite.AuthInfo(args.username, password, self._zk.getInstanceId()) 
		
		self._connector = sharkbite.AccumuloConnector(self._user, self._zk)
		
		self._tableOperations = self._connector.tableOps(self._table)
		
		print("created connector")
		

	def getConnector(self):
		return self._connector
		
	def newtableOperations(self):
		self._tableOperations = self._connector.tableOps(self._table)
		return self._tableOperations
		
	def getTableOperations(self):
		return self._tableOperations
			


