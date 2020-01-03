package org.poma.accumulo;

import org.apache.accumulo.core.data.ByteSequence;
import org.apache.accumulo.core.data.Key;
import org.apache.accumulo.core.data.Range;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.iterators.IteratorEnvironment;
import org.apache.accumulo.core.iterators.SortedKeyValueIterator;
import org.apache.accumulo.core.iterators.WrappingIterator;
import org.python.core.Py;
import org.python.core.PyException;
import org.python.core.PyInstance;
import org.python.util.PythonInterpreter;

import java.io.IOException;
import java.util.Collection;
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
        findTop(null);
    }

    public void findTop(Range range) throws IOException {
        nextKey = null;
        nextValue = null;
        try(PythonInterpreter iterpret = new PythonInterpreter()) {
            iterpret.exec(dsl);
            PyInstance instance = (PyInstance)iterpret.eval(className + "()");

            if (range != null){
                KeyValue kv = null;
                try {
                    kv = (KeyValue) (instance.invoke("seek", Py.java2py(wrappedIterator), Py.java2py(range)).__tojava__(KeyValue.class));
                }catch(PyException e){
                    if (e.getMessage().contains("AttributeError"))
                    {
                        wrappedIterator.seek(range);
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
        }
    }
//private native void seek(WrappedIterator iter,Range range);
    @Override
    public void seek(Range range, Collection<ByteSequence> collection, boolean b) throws IOException {
        List<String> families =  collection.stream().map(x -> x.toString()).collect(Collectors.toList());
        findTop(range);
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
