package org.poma.accumulo;
import org.apache.accumulo.core.data.ByteSequence;
import org.apache.accumulo.core.data.Key;
import org.apache.accumulo.core.data.Range;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.iterators.IteratorEnvironment;
import org.apache.accumulo.core.iterators.SortedKeyValueIterator;
import org.apache.accumulo.core.iterators.WrappingIterator;
import org.python.core.Py;
import org.python.core.PyObject;
import org.python.core.PyType;
import org.python.util.PythonInterpreter;

import java.io.IOException;
import java.util.Collection;
import java.util.List;
import java.util.Map;
import java.util.stream.Collectors;

public class JythonIterator extends WrappingIterator{

    private Map<String,String> options;

    public static final String DSL_TYPE = "DSL_TYPE";
    public static final String DSL_CLASS = "DSL_CLASS";
    public static final String DSL_VALUE = "DSL_VALUE";

    PythonInterpreter iterpret;
    PyObject pyObj = null;
    @Override
    public void init(SortedKeyValueIterator<Key, Value> sortedKeyValueIterator, Map<String, String> options, IteratorEnvironment iteratorEnvironment) throws IOException {
        iterpret = new PythonInterpreter();
        super.init(sortedKeyValueIterator,options,iteratorEnvironment);
        this.options = options;
        String dslClass = options.get(DSL_CLASS);
        String dsl = options.get(DSL_VALUE);
        pyObj = iterpret.eval(dsl);


    }

    @Override
    public boolean hasTop() {
        return (Boolean)pyObj.invoke("hasTop").__tojava__(Boolean.class);
    }

    @Override
    public void next() throws IOException {
        pyObj.invoke("next",Py.java2py(super.getSource()));
    }

    @Override
    public void seek(Range range, Collection<ByteSequence> collection, boolean b) throws IOException {
        List<String> families =  collection.stream().map(x -> x.toString()).collect(Collectors.toList());
        super.seek(range,collection,b);
        //dslIterator.seek(range,families,b);
    }

    @Override
    public Key getTopKey() {
        return (Key)pyObj.invoke("getTopKey").__tojava__(Key.class);
    }

    @Override
    public Value getTopValue() {
        return (Value)pyObj.invoke("getTopValue").__tojava__(Value.class);
    }

    @Override
    public SortedKeyValueIterator<Key, Value> deepCopy(IteratorEnvironment iteratorEnvironment) {
        return new JythonIterator();
    }
}
