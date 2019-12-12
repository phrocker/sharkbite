import sharkbite_iterator

def seek(iterator,soughtRange):
    range = sharkbite_iterator.Range("a")
    iterator.seek(range)


def onNext(iterator):
    if (iterator.hasTop()):
       kv = sharkbite_iterator.KeyValue()
       key = iterator.getTopKey()
       cf = key.getColumnFamily()
       key.setColumnFamily("oh changed " + cf)
       kv.setKey(key,True)
       return kv
    else: 
       return None
