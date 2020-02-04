package org.poma.accumulo;

import org.apache.accumulo.core.data.ArrayByteSequence;
import org.apache.accumulo.core.data.ByteSequence;
import org.apache.accumulo.core.data.Range;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.iterators.IteratorEnvironment;
import org.apache.accumulo.core.iterators.SortedKeyValueIterator;
import pysharkbite.Key;

import java.io.IOException;
import java.util.Collection;
import java.util.Collections;
import java.util.Map;
import java.util.stream.Collectors;

public class WrappedIterator implements SortedKeyValueIterator<Key,Value>{

    private final SortedKeyValueIterator<org.apache.accumulo.core.data.Key, Value> sortedKeyValueIterator;


    WrappedIterator(SortedKeyValueIterator<org.apache.accumulo.core.data.Key, Value> sortedKeyValueIterator){
        this.sortedKeyValueIterator=sortedKeyValueIterator;
    }
    public void seek(Range range, boolean inclusive, Collection<String> fams) throws IOException {
        Collection<ByteSequence> families = fams.stream().map( x -> new ArrayByteSequence(x)).collect(Collectors.toList());
        sortedKeyValueIterator.seek(range,families,inclusive);
    }

    public void seek(Range range, boolean inclusive) throws IOException {
        Collection<ByteSequence> families = Collections.EMPTY_LIST;
        sortedKeyValueIterator.seek(range,families,inclusive);
    }

    public void seek(Range range) throws IOException {
        Collection<ByteSequence> families = Collections.EMPTY_LIST;
        sortedKeyValueIterator.seek(range,families,false);
    }

    /***
     * wrapped methods
     */
    @Override
    public Key getTopKey() {

        return new Key(sortedKeyValueIterator.getTopKey());
    }

    public Value getTopValue() {
        return sortedKeyValueIterator.getTopValue();
    }

    @Override
    public SortedKeyValueIterator<Key, Value> deepCopy(IteratorEnvironment iteratorEnvironment) {
        return null;
    }

    @Override
    public void init(SortedKeyValueIterator<Key, Value> sortedKeyValueIterator, Map<String, String> map, IteratorEnvironment iteratorEnvironment) throws IOException {

    }

    public boolean hasTop() {
        return sortedKeyValueIterator.hasTop();
    }


    public void next() throws IOException {
        sortedKeyValueIterator.next();
    }

    public void seek(Range range, Collection<ByteSequence> columnFamilies, boolean inclusive) throws IOException {
        sortedKeyValueIterator.seek(range,columnFamilies,inclusive);
    }

}
