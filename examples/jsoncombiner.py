class ExampleCombiner: 
## This example iterator assumes the CQ contains a field name
## and value pair separated by a null delimiter
  def onNext(iterator):
    if (iterator.hasTop()):
        kv = sharkbite_iterator.KeyValue()
        mapping = {}
        key = iterator.getTopKey()
        cf = key.getColumnFamily()
        while (iterator.hasTop() and cf == key.getColumnFamily()):
            ## FN and FV in cq
            fieldname = key.getColumnQualifier().split("\u0000")[0];
            fieldvalue = key.getColumnQualifier().split("\u0000")[1];
            mapping[fieldname]=fieldvalue
            iterator.next();
            if (iterator.hasTop()):
                key = iterator.getTopKey()
        json_data = json.dumps(mapping,indent=4, sort_keys=True);
        value = sharkbite_iterator.Value();
        value.setValue(json_data);
        kv.setValue(value);
        kv.setKey(key)
        return kv
    else: 
       return None
