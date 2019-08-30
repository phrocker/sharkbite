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

class CKey(ctypes.Structure):
    _fields_ = [('row', ctypes.c_char_p),
                ('cf', ctypes.c_char_p),
                ('cq', ctypes.c_void_p),
                ('cv', ctypes.c_void_p),
                ('timestamp', ctypes.c_int)]

class CValue(ctypes.Structure):
    _fields_ = [('value', ctypes.c_ubyte),
                ('offset', ctypes.c_short),
                ('valueSize', ctypes.c_int)]

class CKeyValue(ctypes.Structure):
    _fields_ = [('key', ctypes.POINTER(CKey)),
                ('value', ctypes.POINTER(CValue))]



class CRange(ctypes.Structure):
    _fields_ = [('start', ctypes.POINTER(CKey)),
                ('stop', ctypes.POINTER(CKey)),
                ('startKeyInclusive', ctypes.c_int),
                ('stopKeyInclusive', ctypes.c_int),
                ('infiniteStartKey', ctypes.c_int),
                ('infiniteStopKey', ctypes.c_int)]


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


class Key(object):
    def __init__(self, row, cf, cq, cv, timestamp):
        super(Key, self).__init__()
        self.row = row.encode('utf-8')
        self.cf = cf.encode('utf-8')
        self.cq = cq.encode('utf-8')
        self.cv = cv.encode('utf-8')
        self.timestamp = timestamp


class KeyValue(object):
    def __init__(self, keyv):
        super(KeyValue, self).__init__()
        key = keyv.key.contents;
        self.key = Key(key.row,key.cf,key.cq,key.cv,key.timestamp)
        self.value = keyv.value

class Range(object):
    def __init__(self, start, stop, startKeyInclusive, stopKeyInclusive, infiniteStartKey, infiniteStopKey):
        super(Range, self).__init__()
        self.start = start
        self.stop = stop
        self.startKeyInclusive=startKeyInclusive
        self.stopKeyInclusive=stopKeyInclusive
        self.infiniteStartKey=infiniteStartKey
        self.stopKeyInclusive=stopKeyInclusive



class BatchScanner(object):
    def __init__(self, scanner, sharkbite):
        super(BatchScanner, self).__init__()
        self._scanner = scanner
        self._sharkbite = sharkbite

    def addRange(self, range):
        crng = CRange(self._sharkbite.createKey(range.start.row,range.start.cf,range.start.cq,range.start.cv,range.start.timestamp),
                     self._sharkbite.createKey(range.stop.row,range.stop.cf,range.stop.cq,range.stop.cv,range.stop.timestamp))
        self._sharkbite.addRange(self._scanner, crng)

    def hasNext(self):
        return self._sharkbite.hasNext(self._scanner)

    def nextKeyValue(self):
        cv = self._sharkbite.next(self._scanner)
        if cv.contents is None:
            return None
        else:
            return KeyValue(cv.contents)

    def __del__(self):
        self._sharkbite.closeScanner(self._scanner)



class AccumuloConnector(object):
    """ Proxy Connector """
    def __init__(self, dll_file, instance="localhost", zookeepers="localhost:2181", user='root', password='secret', _connect=True):
        super(AccumuloConnector, self).__init__()
        self.sharkbite = cdll.LoadLibrary(dll_file)

        if _connect:
            self.sharkbite.create_connector.restype = ctypes.POINTER(Connector)
            self.connector = self.sharkbite.create_connector(instance.encode('utf-8'), zookeepers.encode('utf-8'), user.encode('utf-8'), password.encode('utf-8'))
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
            self.sharkbite.closeWriter.restype = ctypes.c_int
            """ createKey """
            self.sharkbite.createKey.argtypes = [ ctypes.c_char_p, ctypes.c_char_p, ctypes.c_char_p , ctypes.c_char_p , ctypes.c_int]
            self.sharkbite.createKey.restype = ctypes.POINTER(CKey)
            """ addRange """
            self.sharkbite.addRange.argtypes = [ ctypes.POINTER(Scanner), ctypes.POINTER(CRange) ]
            self.sharkbite.addRange.restype = ctypes.c_int
            """ hasNext """
            self.sharkbite.hasNext.argtype = ctypes.POINTER(Scanner)
            self.sharkbite.hasNext.restype = ctypes.c_bool
            """ next """
            self.sharkbite.next.argtype = ctypes.POINTER(Scanner)
            self.sharkbite.next.restype = ctypes.POINTER(CKeyValue)

            
    def create_table(self, table):
         """ create table """
         tableOps = self.sharkbite.create_table(self.connector, table)
         return tableOps
    
    def drop_table(self,tableOps):
         """ create table """
         self.sharkbite.drop_table(tableOps)
         
    def create_scanner(self, tableOps, threads=2):
        scanner = self.sharkbite.createScanner(tableOps,threads)
        returned_scanner = BatchScanner(scanner=scanner, sharkbite=self.sharkbite)
        return returned_scanner

    def create_writer(self, tableOps, threads=2):
        writer = self.sharkbite.createWriter(tableOps,threads)
        returned_writer = BatchWriter(writer=writer, sharkbite=self.sharkbite)
        return returned_writer
