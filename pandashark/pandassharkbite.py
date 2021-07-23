from . import AuthInfo, Range, Configuration, ZookeeperInstance, AccumuloConnector, Authorizations, Key, AccumuloBase, AccumuloIterator
import pandas
class AccumuloIterator(AccumuloBase):

    _scanner = None
    _iter = None
    _resultset = None
    _chunkSize = 0
    _chunkCounter = 0

    def __init__(self, scanner):
        self._scanner = scanner
        self._resultset = scanner.getResultSet()
    
    def __init__(self, scanner, chunkSize):
        self._scanner = scanner
        self._resultset = scanner.getResultSet()
        self._chunkSize = chunkSize

    def __iter__(self):
        self._iter = self._resultset.__iter__()
        return self

    def nextBatch(self):
        self._chunkCounter = 0

    def __next__(self):
        tk = self._iter.__next__()
        if tk is None or (self._chunkSize > 0 and self._chunkCounter >= self._chunkSize ):
            raise StopIteration
        else:
            self._chunkCounter += 1
            return tk
        

class DataFrameIterator(AccumuloIterator):

    _columns = set()
    _iterator  = None
    def __init__(self, iterator: AccumuloIterator):
        self.__iterator = iterator
        self._columns.add("row")
        self._columns.add("column")
        self._columns.add("visibility")
        self._columns.add("value")        
    
    def __iter__(self):
        self.__iterator.__iter__()
        return self

    def get_columns(self):
        return self._columns

    def _make_dataframe(self, datas,rows):
        df = pandas.DataFrame(datas, index=rows)
        nan_value = float("NaN")
        df.replace("", nan_value, inplace=True)
        df.dropna(how='all', axis=1, inplace=True)
        return df
    def __next__(self):
        tk = None
        datums = []
        rows = []
        while True:
            try:
                tk = self.__iterator.__next__()
            except StopIteration:
                self.__iterator.nextBatch()
                return self._make_dataframe(datums,rows)
            column = tk.getKey().getColumnFamily() + ":" + tk.getKey().getColumnQualifier()
            datums.append({'column' : column , 'visibility' : tk.getKey().getColumnVisibility(), 'timestamp' : tk.getKey().getTimestamp(), 'value' : tk.getValue().get()})
            rows.append(tk.getKey().getRow())
        return self._make_dataframe(datums,rows)