key = None
def getTopKey():
  return key
def getTopValue():
  return None
def seek(iterator,soughtRange):
    iterator.seek(soughtRange)
    if (iterator.hasTop()):
        key = iterator.getTopKey()
        if key is None:
            print("oh empty")
        else:
            print(key.getRow())
def hasTop():
    return key is None
def getTopValue():
  return None
def next(iterator):
    iterator.next()
    if (iterator.hasTop()):
        key = iterator.getTopKey()
        key.setColumnFamily("ohhh")
    else:
        key = None
