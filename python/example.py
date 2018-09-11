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

""" dll_file is the path to the shared object """
conn = Conector(dll_file=args.dll_file, instance = args.instance, zookeepers = args.zookeepers, user = args.username, password = args.password)

tabletocreate = "blahblah3".encode('utf-8')

tableOps = conn.create_table(tabletocreate)

writer = conn.create_writer(tableOps,2)

mutation = writer.createMutation("blah")

mutation.put("cf","cq","")

mutation.put("cf2","cq","")

writer.addMutation(mutation)

start = Key("a", "", "" , "" ,235325235 )
stop = Key("d", "", "" , "" ,235325235 )

scanRange = Range(start, stop, 0 , 0 , 1, 1)

scanner = conn.create_scanner(tableOps,2)

scanner.addRange(scanRange)

if scanner.hasNext():
    ck = scanner.nextKeyValue()
    print(ck.key.row)





writer.close()
