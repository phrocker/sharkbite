package org.poma.accumulo;

import org.apache.accumulo.core.data.Key;
import org.apache.accumulo.core.data.Range;
import org.apache.accumulo.core.data.Value;

import java.io.IOException;
import java.util.List;
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

    public void setTopKey(Key key){
        this.nextKey = key;
    }

    public void setTopValue(Value value){
        this.nextValue = value;
    }

    public void next( WrappedIterator iter)throws IOException{
        nextKey = null;
        nextValue = null;
        getNextKey(iter);
    }


    private native void getNextKey(WrappedIterator iter) throws IOException;

    private native void seek(WrappedIterator iter,Range range);

    public Key getTopKey(){
        return nextKey;
    }

    public Value getTopValue(){
        if (nextValue == null){
            nextValue = new Value();
        }
        return nextValue;
    }

    public void seek(WrappedIterator source, Range range, List<String> families, boolean b) {
        seek(source,range);
    }
}
