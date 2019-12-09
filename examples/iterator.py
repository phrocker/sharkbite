key = None
def getTopKey():
  return key
def getTopValue():
  return None
def hasTop():
  return key is None
def getTopValue():
  return None
def next(iterator):
  iterator.next()
  key = iterator.getTopKey()
  if key is None:
    print("oh empty")
  else:
    print(key.getRow())
