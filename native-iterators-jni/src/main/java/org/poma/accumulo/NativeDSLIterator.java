package org.poma.accumulo;

import org.apache.accumulo.core.data.ByteSequence;
import org.apache.accumulo.core.data.Key;
import org.apache.accumulo.core.data.Range;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.iterators.IteratorEnvironment;
import org.apache.accumulo.core.iterators.SortedKeyValueIterator;
import org.apache.accumulo.core.iterators.WrappingIterator;

import java.io.IOException;
import java.util.Collection;
import java.util.List;
import java.util.Map;
import java.util.stream.Collectors;

public class NativeDSLIterator extends WrappingIterator{

    private DSLIterator dslIterator = null;
    private Map<String,String> options;

    public static final String DSL_TYPE = "DSL_TYPE";
    public static final String DSL_VALUE = "DSL_VALUE";

    static {
            try {
                JNILoader.load();
            } catch (Exception e) {
                // TODO Auto-generated catch block
                e.printStackTrace();
            }
    }

    @Override
    public void init(SortedKeyValueIterator<Key, Value> sortedKeyValueIterator, Map<String, String> options, IteratorEnvironment iteratorEnvironment) throws IOException {
        if (!JNILoader.isLoaded()){
            throw new RuntimeException("Could not load JNI library");
        }
        super.init(sortedKeyValueIterator,options,iteratorEnvironment);
        this.options = options;
        dslIterator = new DSLIterator();
        dslIterator.init(options);
        String dslType = options.get(DSL_TYPE);
        String dsl = options.get(DSL_VALUE);
        dslIterator.setType(dslType);
        dslIterator.setDSL(dsl);
    }

    @Override
    public boolean hasTop() {
        return dslIterator.hasTop();
    }

    @Override
    public void next() throws IOException {
        dslIterator.next(super.getSource());
    }

    @Override
    public void seek(Range range, Collection<ByteSequence> collection, boolean b) throws IOException {
        List<String> families =  collection.stream().map(x -> x.toString()).collect(Collectors.toList());
        super.seek(range,collection,b);
        //dslIterator.seek(range,families,b);
    }

    @Override
    public Key getTopKey() {
        return dslIterator.getTopKey();
    }

    @Override
    public Value getTopValue() {
        return dslIterator.getTopValue();
    }

    @Override
    public SortedKeyValueIterator<Key, Value> deepCopy(IteratorEnvironment iteratorEnvironment) {
        return new NativeDSLIterator();
    }
}
