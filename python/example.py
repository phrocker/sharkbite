from accumulo import *

from argparse import ArgumentParser
from ctypes import cdll
import ctypes



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

sharkbite = cdll.LoadLibrary(args.dll_file)


conn = Accumulo(dll_file=args.dll_file,instance = args.instance, zookeepers = args.zookeepers, user = args.username, password = args.password)

tabletocreate = "blahblah3".encode('utf-8')

"""
sharkbite.create_table.argtypes = [ctypes.POINTER(Connector), ctypes.c_char_p ]
sharkbite.create_table.restype = ctypes.POINTER(TableOps)


tableOps = sharkbite.create_table(connector, tabletocreate)
"""
tableOps = conn.create_table(tabletocreate)

scanner = conn.create_scanner(tableOps)

conn.drop_table(tableOps) 

