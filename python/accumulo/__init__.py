#!/usr/bin/env python
# Licensed to the Apache Software Foundation (ASF) under one or more
# contributor license agreements.  See the NOTICE file distributed with
# this work for additional information regarding copyright ownership.
# The ASF licenses this file to You under the Apache License, Version 2.0
# (the "License"); you may not use this file except in compliance with
# the License.  You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
from ctypes import cdll
import ctypes

class Connector(ctypes.Structure):
    _fields_ = [('masterPtr', ctypes.c_voidp),
                ('zk', ctypes.c_voidp)]


class TableOps(ctypes.Structure):
    _fields_ = [('table_name', ctypes.c_char_p),
                ('parent', ctypes.POINTER(Connector)),
                ('tableOpsPtr', ctypes.c_void_p)]

class Scanner(ctypes.Structure):
    _fields_ = [('tableOps', ctypes.POINTER(TableOps)),
                ('scannerPtr', ctypes.c_void_p),
                ('res', ctypes.c_void_p),
                ('on_next_fn', ctypes.c_void_p)]

class MutationStruct(ctypes.Structure):
    _fields_ = [('mutationPtr', ctypes.c_void_p)]

class Writer(ctypes.Structure):
    _fields_ = [('writerPtr', ctypes.c_void_p)]

class Mutation(object):
    def __init__(self, mut, sharkbite):
        super(Mutation, self).__init__()
        self.mut = mut
        self._sharkbite = sharkbite

    def put(self, cf='', cq='', cv=None, ts=None, val='', is_delete=None):
        self._sharkbite.put(self.mut, cf,cq,cv)

class BatchWriter(object):
    def __init__(self, writer, sharkbite):
        super(BatchWriter, self).__init__()
        self._writer = writer
        self._sharkbite = sharkbite

    def createMutation(self, row):
        mut = self._sharkbite.createMutation(row)
        m = Mutation(mut,self._sharkbite)
        return m

    def addMutation(self, mutation):
        self._sharkbite.addMutation(self._writer, mutation.mut)

    def close(self,):
        self._sharkbite.closeWriter(self._writer)





class Conector(object):
    """ Proxy Connector """
    def __init__(self, dll_file, instance="localhost", zookeepers="localhost:2181", user='root', password='secret', _connect=True):
        super(Conector, self).__init__()
        self.sharkbite = cdll.LoadLibrary(dll_file)

        if _connect:
            self.sharkbite.create_connector.restype = ctypes.POINTER(Connector)
            self.connector = self.sharkbite.create_connector(instance, zookeepers, user, password)
            self.sharkbite.create_table.argtypes = [ctypes.POINTER(Connector), ctypes.c_char_p ]
            self.sharkbite.create_table.restype = ctypes.POINTER(TableOps)
            self.sharkbite.drop_table.argtype = ctypes.POINTER(TableOps)
            self.sharkbite.createScanner.argtypes = [ ctypes.POINTER(TableOps), ctypes.c_short ]
            self.sharkbite.createScanner.restype = ctypes.POINTER(Scanner) 
            self.sharkbite.closeScanner.argtype = ctypes.POINTER(Scanner)
            """ createWriter """
            self.sharkbite.createWriter.argtype = ctypes.c_char_p
            self.sharkbite.createWriter.restype = ctypes.POINTER(Writer)
            """ createMutation """
            self.sharkbite.createMutation.argtype = ctypes.c_char_p
            self.sharkbite.createMutation.restype = ctypes.POINTER(MutationStruct)
            """ put """
            self.sharkbite.put.argtype = ctypes.c_char_p
            """ add mutation """
            self.sharkbite.addMutation.argtypes = [ ctypes.POINTER(Writer), ctypes.POINTER(MutationStruct)]
            self.sharkbite.addMutation.restype = ctypes.c_short
            """ close """
            self.sharkbite.closeWriter.argtype = ctypes.POINTER(Writer)
            self.sharkbite.closeWriter.restype = ctypes.c_short

            
    def create_table(self, table):
         """ create table """
         tableOps = self.sharkbite.create_table(self.connector, table)
         return tableOps
    
    def drop_table(self,tableOps):
         """ create table """
         self.sharkbite.drop_table(tableOps)
         
    def create_scanner(self, tableOps, threads=2):
        scanner = self.sharkbite.createScanner(tableOps,threads)
        returned_scanner = BatchScanner(scanner=scanner, sharkbite=self.sharkbite, threads=threads)
        return returned_scanner

    def create_writer(self, tableOps, threads=2):
        writer = self.sharkbite.createWriter(tableOps,threads)
        returned_writer = BatchWriter(writer=writer, sharkbite=self.sharkbite)
        return returned_writer
 

class BatchScanner(object):
    """docstring for BatchScanner"""
    def __init__(self, scanner, sharkbite, max_memory=10*1024, latency_ms=30*1000, timeout_ms=5*1000, threads=2):
        super(BatchScanner, self).__init__()
        self._scanner = scanner
        self._sharkbite = sharkbite
        self._is_closed = False
        
    
        
    def __del__(self):
        self._sharkbite.closeScanner(self._scanner)
        
        
        
        
        
        
        
        
        