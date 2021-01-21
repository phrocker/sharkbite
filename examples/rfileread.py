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

parser.add_argument("-f", "--file", dest="file",
                    help="path to RFile", required=True)

args = parser.parse_args()

file = args.file

import sharkbite
try:

   rfile = sharkbite.RFileOperations.open(file)

   # list of column families
   cf = list()

   ## range

   rng = sharkbite.Range()

   ## seekable is an object that replaces the function call
   ## of seek(range,cf,boolean). This exists because the extension point
   ## in the C++ Api allows varying seekable objects that change how seeks occur
   seek = sharkbite.Seekable(rng,cf,False)

   rfile.seek(seek)

   ## print the row
   while rfile.hasNext():
       kv = rfile.getTop()
       print(kv.getKey())
       rfile.next()

except RuntimeError as e:
     traceback.print_exc()
     print("Oops, error caused: " + str(e))
