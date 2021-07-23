import pkgutil
import time

__path__ = pkgutil.extend_path(__path__, __name__)


from contextlib import contextmanager
from datetime import (
    date,
    datetime,
    time,
)
from functools import partial
import re
from typing import (
    Any,
    Iterator,
    Sequence,
    cast,
    overload,
)
import warnings

import numpy as np

import pandas._libs.lib as lib
from pandas.compat._optional import import_optional_dependency
from pandas.errors import AbstractMethodError

from pandas.core.dtypes.common import (
    is_datetime64tz_dtype,
    is_dict_like,
    is_list_like,
)
from pandas.core.dtypes.dtypes import DatetimeTZDtype
from pandas.core.dtypes.missing import isna

from pandas import get_option
from pandas.core.api import (
    DataFrame,
    Series,
)
from pandas.core.base import PandasObject
from pandas.core.tools.datetimes import to_datetime


from sharkbite import AuthInfo, Range, Configuration, ZookeeperInstance, AccumuloConnector, AccumuloScanner, Authorizations, AccumuloIterator, Key, AccumuloBase
from . import pandassharkbite


@overload
def read_accumulo(
    connector : AccumuloBase,
    ranges : list,
    columns=None,
    index_col=None,
    chunksize : int= 1000
):
    scanner = connector.to_scanner()
    scanner.fetch_ranges(ranges)
    return read_accumulo(scanner,columns,index_col,chunksize)


def read_accumulo(
    scanner : AccumuloScanner,
    columns=None,
    index_col=None,
    chunksize : int= 1000
):
    """
    

    Parameters
    ----------
    sql : AccumuloBase
    columns : list, default: None
        List of column names to select from SQL table (only used when reading
        a table).
    chunksize : int, default None
        If specified, return an iterator where `chunksize` is the
        number of rows to include in each chunk.

    Returns
    -------
    DataFrame or Iterator[DataFrame]

    """
    ## assume that scanner is established previously
    print(f"chunk size is {chunksize}")
    if columns is not None:
        for column in columns:
            scanner.select_column(column)
    iterator = pandassharkbite.DataFrameIterator(scanner.get(chunksize))
    iterator.__iter__()
    cols = iterator.get_columns()

    frame = iterator.__next__()
    return frame

def read_accumulo_nex(
    iterator : AccumuloIterator,
    columns=None,
    index_col=None,
    chunksize : int= 1000
):
    """
        
    """
    frame = iterator.__next__()
    return frame

