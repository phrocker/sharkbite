from ctypes import cdll
from argparse import ArgumentParser
from ctypes import cdll
import ctypes
import os
import traceback
import sys
import time
from testmodule import *
from tests import *




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
table = args.table
dll = args.shardobject

print ("Opening ",dll)
py = cdll.LoadLibrary(dll)

import pysharkbite

conf = pysharkbite.Configuration()

conf.set ("FILE_SYSTEM_ROOT", "/accumulo");

zk = pysharkbite.ZookeeperInstance(args.instance, args.zookeepers, 1000, conf)

user = pysharkbite.AuthInfo(args.username, password, zk.getInstanceId()) 

try:
	connector = pysharkbite.AccumuloConnector(user, zk)

	for cls in TestRunner.__subclasses__():
		print(cls.__name__)
	
    
except RuntimeError as e:
	traceback.print_exc()
	print("Oops, error caused: " + str(e))