class myIterator: 
  def seek(self,iterator,soughtRange):
    range = Range("a")
    iterator.seek(range)


  def onNext(self,iterator):
    if (iterator.hasTop()):
    	kv = KeyValue()
  	  key = iterator.getTopKey()
  	  cf = key.getColumnFamily()
  	  value = iterator.getTopValue()
  	  key.setColumnFamily("oh changed " + cf)
  	  iterator.next()
  	  return KeyValue(key,value)
    else: 
      return None