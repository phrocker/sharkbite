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
    _table_operations = None
    _auths = None
    _threads=10

    def to_scanner(self):
        return AccumuloScanner(self._instance,self._zookeepers,self._username,self._password,self._table,self._auths)

    def __init__(self, instance: str , zookepeers: str, username: str, password: str, table: str =None, auths: str =None):
        self._conf = Configuration()
        self._instance = instance
        self._zookeepers = zookepeers
        self._username = username
        self._password = password
        self._zk =  ZookeeperInstance(self._instance, self._zookeepers, 1000, self._conf)
        self._user = AuthInfo(self._username, self._password , self._zk.getInstanceId()) 
        self.set_authorizations(auths)
        self._connector = AccumuloConnector(self._user, self._zk)
        self._table = table
        if self._table is not None:
            self._table_operations = self._connector.tableOps(self._table)


    def list_tables(self):
        return self._connector.tableInfo().list_tables()


    def set_authorizations(self,auths):
        if auths is not None and isinstance(auths, Authorizations):
            self._auths = auths
        else:
            if len(auths) > 0:
                self._auths = Authorizations(auths)
            else:
                self._auths = Authorizations()

    def set_threads(self, thread_count):
        self._threads=thread_count

    def set_table(self,table: str):
        if table is not None:
            self._table = table
            self._table_operations = self._connector.tableOps(self._table)

class AccumuloScanner(AccumuloBase):
    
    def __init__(self, instance, zookepeers, username, password, table, auths):
        pass

class AccumuloWriter(AccumuloBase):
    _row = ""
    _mutation = None
    _writer = None
    
    def __init__(self,  instance: str , zookeepers: str, username: str, password: str, table: str =None, auths: str =None):
        super().__init__(instance,zookeepers,username,password,table,auths)

    def put(self,row : str , cf: str , cq: str , cv: str =None, timestamp: int =0, value=None):
        if self._writer is None:
            self._writer = self._table_operations.createWriter(self._auths,self._threads)
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
    
    def putDelete(self,row : str , cf: str , cq: str , cv: str ="", timestamp: int =0):
        if self._writer is None:
            self._writer = self._table_operations.createWriter(self._auths,self._threads)
        if row != self._row:
            self._row = row
            if self._mutation is not None:
                self._writer.addMutation(self._mutation)
            self._mutation = Mutation(row)
        if cf is None or cq is None:
            raise ValueError("CF and CQ must be specified")
        ts = timestamp
        if ts > 0:
            self._mutation.putDelete(cf,cq,cv,ts) 
        else:
            self._mutation.putDelete(cf,cq,cv) 
    
    def delete(self, key : Key):
        if key is not None:
            self.putDelete(key.getRow(),key.getColumnFamily(),key.getColumnQualifier(),key.getColumnVisibility(), key.getTimestamp())

    def close(self):
        if self._writer is not None: 
            if self._mutation is not None:
                self._writer.addMutation(self._mutation)
            self._writer.close()
            self._writer=None
            self._row=None
            self._mutation = None

    def __del__(self):
        self.close()
            

class AccumuloScanner(AccumuloBase):
    _row = ""
    _scanner = None
    
    def __init__(self, instance: str , zookeepers: str, username: str, password: str, table: str =None, auths: str =None):
        super().__init__(instance,zookeepers,username,password,table,auths)
        

    def get(self,begin_row, end_row=None):
        if end_row is None:
            range = Range(begin_row)
        else:
            range = Range(begin_row,True,end_row,False)
    

        scanner = self._table_operations.createScanner(self._auths,self._threads)

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
        