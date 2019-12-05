package org.poma.accumulo;

import org.apache.accumulo.core.data.Key;
import org.apache.accumulo.core.data.Range;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.iterators.SortedKeyValueIterator;

import java.io.IOException;
import java.util.Collection;
import java.util.Map;

public class DSLIterator {

    public long nativePtr;

    Key nextKey = null;

    Value nextValue = null;

    public DSLIterator(){
    }

    /**
     *
     * local methods
     */

    public native void setType(final String type);

    public native void setDSL(final String DSL);

    public native void init(Map<String, String> options);

    public boolean hasTop(){
        return nextKey != null;
    }

    public void next( SortedKeyValueIterator<Key, Value> iter)throws IOException{
        Map.Entry<Key,Value> kv = getNextKey(iter);
        if (null != kv){
            nextKey = kv.getKey();
            nextValue = kv.getValue();
        }
        else{
            System.out.println("oh done");
            nextKey = null;
            nextValue = null;
        }
    }

    private native Map.Entry<Key,Value> getNextKey( SortedKeyValueIterator<Key, Value> iter) throws IOException;

    //public native void seek(Range range, Collection<String> families, boolean inclusive) throws IOException;

    public Key getTopKey(){
        return nextKey;
    }

    public Value getTopValue(){
        return nextValue;
    }

}
