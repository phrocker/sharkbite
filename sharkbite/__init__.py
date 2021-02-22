import pkgutil
import time
__path__ = pkgutil.extend_path(__path__, __name__)

from .pysharkbite import *

class AccumuloBase:
    _conf = None
    _instance = ""
    _zookeepers = ""
    _password = ""
    _username = ""
    _zk = None
    _user = None
    _connector = None
    _table = ""
    _tableOperations = None
    _auths = None

    def to_scanner(self):
        return AccumuloScanner(self._instance,self._zookeepers,self._username,self._password,self._table,self._auths)

    def __init__(self):
        pass

class AccumuloScanner(AccumuloBase):
    
    def __init__(self, instance, zookepeers, username, password, table, auths):
        pass

class AccumuloWriter(AccumuloBase):
    _row = ""
    _mutation = None
    _writer = None
    
    def __init__(self, instance, zookepeers, username, password, table, auths):
        self._conf = Configuration()
        self._instance = instance
        self._zookeepers = zookepeers
        self._username = username
        self._password = password
        self._zk =  ZookeeperInstance(self._instance, self._zookeepers, 1000, self._conf)
        self._user = AuthInfo(self._username, self._password , self._zk.getInstanceId()) 
        if isinstance(auths, Authorizations):
            self._auths = auths
        else:
            if len(auths) > 0:
                self._auths = Authorizations(auths)
            else:
                self._auths = Authorizations()
        self._connector = AccumuloConnector(self._user, self._zk)
        self._table = table
        self._tableOperations = self._connector.tableOps(self._table)
        self._writer = self._tableOperations.createWriter(self._auths,10)

    def put(self,row, cf, cq, cv=None, timestamp=0, value=None):
        if row != self._row:
            self._row = row
            if self._mutation is not None:
                self._writer.addMutation(self._mutation)
            self._mutation = Mutation(row)
        if cf is None or cq is None:
            raise ValueError("CF and CQ must be specified")
        ts = timestamp
        if ts == 0:
            ts = milliseconds = int(round(time.time() * 1000))
        if value is None:
            if cv is None:
                self._mutation.put(cf=cf,cq=cq,timestamp=ts) 
            else:
                self._mutation.put(cf=cf,cq=cq,cv=cv,timestamp=ts) 
        else:
            if cv is None:
                self._mutation.put(cf=cf,cq=cq,cv="",timestamp=ts,value=value) 
            else:
                self._mutation.put(cf=cf,cq=cq,cv=cv,timestamp=ts,value=value) 
    
    def __del__(self):
        if self._mutation is not None:
            self._writer.addMutation(self._mutation)
        self._writer.close()

class AccumuloScanner(AccumuloBase):
    _row = ""
    _scanner = None
    
    def __init__(self, instance, zookepeers, username, password, table, auths):
        self._conf = Configuration()
        self._instance = instance
        self._zookeepers = zookepeers
        self._username = username
        self._password = password
        self._zk =  ZookeeperInstance(self._instance, self._zookeepers, 1000, self._conf)
        self._user = AuthInfo(self._username, self._password , self._zk.getInstanceId()) 
        if isinstance(auths, Authorizations):
            self._auths = auths
        else:
            if len(auths) > 0:
                self._auths = Authorizations(auths)
            else:
                self._auths = Authorizations()
        self._connector = AccumuloConnector(self._user, self._zk)
        self._table = table
        self._tableOperations = self._connector.tableOps(self._table)
        

    def get(self,begin_row, end_row=None):
        if end_row is None:
            range = Range(begin_row)
        else:
            range = Range(begin_row,True,end_row,False)
    

        scanner = self._tableOperations.createScanner(self._auths,10)

        scanner.addRange( range )
    
        return AccumuloIterator(scanner)

    def __del__(self):
        if self._scanner is not None:
            self._scanner.close()

class AccumuloIterator(AccumuloBase):

    _scanner = None
    _iter = None
    _resultset = None

    def __init__(self, scanner):
        self._scanner = scanner
        self._resultset = scanner.getResultSet()

    def __iter__(self):
        self._iter = self._resultset.__iter__()
        return self

    def __next__(self):
        tk = self._iter.__next__()
        if tk is None:
            raise StopIteration
        else:
            return tk
        