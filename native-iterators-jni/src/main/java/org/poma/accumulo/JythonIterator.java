package org.poma.accumulo;

import org.apache.accumulo.core.data.ByteSequence;
import org.apache.accumulo.core.data.Key;
import org.apache.accumulo.core.data.Range;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.iterators.IteratorEnvironment;
import org.apache.accumulo.core.iterators.SortedKeyValueIterator;
import org.apache.accumulo.core.iterators.WrappingIterator;
import org.apache.commons.pool2.impl.GenericObjectPool;
import org.apache.commons.pool2.impl.GenericObjectPoolConfig;
import org.python.core.Py;
import org.python.core.PyException;
import org.python.core.PyInstance;
import org.python.core.PyObject;
import org.python.util.PythonInterpreter;
import pysharkbite.KeyValue;

import java.io.IOException;
import java.util.Collection;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.stream.Collectors;

public class JythonIterator extends WrappingIterator{

    private Map<String,String> options;

    public static final String DSL_CLASS = "DSL_CLASS";
    public static final String DSL_VALUE = "DSL_VALUE";


    org.apache.accumulo.core.data.Key nextKey = null;

    Value nextValue = null;

    private WrappedIterator wrappedIterator;
    private String dsl;
    private String className;

    static GenericObjectPool<PythonInterpreter> pool = null;
    static {
        GenericObjectPoolConfig<PythonInterpreter> config = new GenericObjectPoolConfig<>();
        config.setMaxIdle(10);
        config.setMaxTotal(20);
        config.setMinIdle(3);
        pool = new GenericObjectPool( new PythonInterpreterFactory(),config);
    }

    @Override
    public void init(SortedKeyValueIterator<Key, Value> sortedKeyValueIterator, Map<String, String> options, IteratorEnvironment iteratorEnvironment) throws IOException {
        super.init(sortedKeyValueIterator,options,iteratorEnvironment);
        this.options = options;
        dsl = options.get(DSL_VALUE);
        className = options.get(DSL_CLASS);
        wrappedIterator = new WrappedIterator(super.getSource());

    }

    @Override
    public boolean hasTop() {
        return nextKey != null;
    }

    @Override
    public void next() throws IOException {
        findTop(null,false);
    }

    public void findTop(Range range, boolean inclusive) throws IOException {
        findTop(range, Collections.EMPTY_LIST, inclusive);
    }

    public void findTop(Range range, Collection<String> families, boolean inclusive) throws IOException {
        nextKey = null;
        nextValue = null;
        PythonInterpreter interpreter = null;
        try {
            interpreter = pool.borrowObject();
            interpreter.exec(dsl);
            PyInstance instance = (PyInstance)interpreter.eval(className + "()");

            if (range != null){
                KeyValue kv = null;
                try {
                    final PyObject objs [] = {Py.java2py(wrappedIterator), Py.java2py(range),  Py.java2py(families),  Py.java2py(inclusive) };
                    kv = (KeyValue) (instance.invoke("seek", objs).__tojava__(KeyValue.class));
                }catch(PyException e){
                    if (e.getMessage().contains("AttributeError"))
                    {
                        wrappedIterator.seek(range,inclusive,families);
                    }
                    else{
                        throw e;
                    }
                }

                boolean hasTopKey = wrappedIterator.hasTop();
                if (kv != null) {
                    nextKey = kv.getKey().toAccumuloKey();
                    nextValue = kv.getValue();
                } else {
                    if (hasTopKey) {
                        nextKey = wrappedIterator.getTopKey().toAccumuloKey();
                        nextValue = wrappedIterator.getTopValue();
                    }
                }
            }
            else{
            boolean hasTopKey =  wrappedIterator.hasTop();


            if (hasTopKey){
                KeyValue kv = (KeyValue)(instance.invoke("onNext",Py.java2py(wrappedIterator)).__tojava__(KeyValue.class));
                if (kv != null){
                    nextKey = kv.getKey().toAccumuloKey();
                    nextValue = kv.getValue();
                }
            }
            }
        } catch (Exception e) {
            // wrap as the end result will always be an i/o exception
            throw new IOException(e);
        }
        finally{
            if (null != interpreter){
                pool.returnObject(interpreter);
            }
        }
    }
//private native void seek(WrappedIterator iter,Range range);
    @Override
    public void seek(Range range, Collection<ByteSequence> collection, boolean inclusive) throws IOException {
        List<String> families =  collection.stream().map(x -> new String( x.getBackingArray() )).collect(Collectors.toList());
        findTop(range,families,inclusive);
    }

    @Override
    public Key getTopKey() {
        return nextKey;
    }

    @Override
    public Value getTopValue() {
        return nextValue;
    }

    @Override
    public SortedKeyValueIterator<Key, Value> deepCopy(IteratorEnvironment iteratorEnvironment) {
        return new JythonIterator();
    }
}
