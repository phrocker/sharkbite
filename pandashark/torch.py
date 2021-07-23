from torch.utils.data import Dataset, IterableDataset, DataLoader
from itertools import cycle, islice
from . import AuthInfo, Range, Configuration, ZookeeperInstance, AccumuloConnector, Authorizations, Key, AccumuloBase

class AccumuloCluster(AccumuloBase, IterableDataset):
    def __init__(self,  instance: str , zookeepers: str, username: str, password: str, table: str =None, auths: str =None):
        super().__init__(instance,zookeepers,username,password,table,auths)


class AccumuloDataset(AccumuloBase,IterableDataset):
  _scanner = None
  _lambda = None
  'Characterizes a dataset for PyTorch'
  def __init__(self, instance: str , zookeepers: str, username: str, password: str, table: str, auths : str, start_key_string: str , end_key_string : str, user_lambda = None):
        'Initialization'
        super().__init__(instance,zookeepers,username,password,table,auths)
        self._range = Range(start_key_string,True,end_key_string, False)
        self.iter = None
        self.iter_iter = None
        self._scanner = self._table_operations.createScanner(self._auths, self._threads)
        self._scanner.addRange( self._range ) 
        self._lambda = user_lambda

  def coerce(self,key):
      if self._lambda is None:
        try:
          return int(key.getKey().getValue().get() )
        except Exception as e:
          print(e)
          return 0
      else:
        try:
          return self._lambda(key)
        except Exception as e:
          print(e)
          return 0

  def __next__(self):
      if self.iter is None:
        raise StopIteration
      cur = None
      try:
        # coerce the key into a relavent type
        cur = self.coerce( self.iter_iter.__next__() )
        if cur is None:
            self.iter=None
            self.iter_iter=None
            raise StopIteration
      except StopIteration:
        self._scanner.close()
        self.iter=None
        self.iter_iter=None
        raise StopIteration
      return cur

  def __iter__(self):
        'Sets the Accumulo Iterator'
        if self.iter is None:
            self.iter = self._scanner.getResultSet()
            self.iter_iter = self.iter.__iter__()
        return self


class AccumuloValueDataset(AccumuloDataset):
    def __init__(self, cluster, table, start_key_string, end_key_string):
        super.__init__(cluster,table,start_key_string,end_key_string)

    def coerce(self,key):
      return key.getValue().get()