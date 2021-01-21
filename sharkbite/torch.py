from torch.utils.data import Dataset, IterableDataset, DataLoader
from itertools import cycle, islice
from . import AuthInfo, Range, Configuration, ZookeeperInstance, AccumuloConnector, Authorizations, Key

class AccumuloCluster(IterableDataset):
    def __init__(self, zk_string, instance_name, user, password):
        self.configuration = Configuration()
        self.zk = ZookeeperInstance(instance_name, zk_string, 1000, self.configuration)
        self.authinfo = AuthInfo(user,password, self.zk.getInstanceId())
        self.connector = AccumuloConnector(self.authinfo, self.zk)
        self.threads=10
        self.auths = Authorizations()

    def set_authorizations(self,auths):
        self.auths=Authorizations(auths)

    def set_threads(self, thread_count):
        self.threads=thread_count


class AccumuloDataset(IterableDataset):
  'Characterizes a dataset for PyTorch'
  def __init__(self, cluster, table, start_key_string, end_key_string):
        'Initialization'
        self._range = Range(start_key_string,True,end_key_string, False)
        self._cluster = cluster
        self.iter = None
        self.iter_iter = None
        self.table_ops = self._cluster.connector.tableOps(table)
        self.scanner = self.table_ops.createScanner(cluster.auths, self._cluster.threads)
        self.scanner.addRange( self._range ) 

  def coerce(self,key):
      
      try:
        return int(key.getKey().getValue().get() )
      except Exception:
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
        self.scanner.close()
        self.iter=None
        self.iter_iter=None
        raise StopIteration
      return cur

  def __iter__(self):
        'Sets the Accumulo Iterator'
        if self.iter is None:
            self.iter = self.scanner.getResultSet()
            self.iter_iter = self.iter.__iter__()
        return self


class AccumuloValueDataset(AccumuloDataset):
    def __init__(self, cluster, table, start_key_string, end_key_string):
        super.__init__(cluster,table,start_key_string,end_key_string)

    def coerce(self,key):
      return key.getValue().get()