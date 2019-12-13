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
import java.util.Collections;
import java.util.Map;

public class WrappedIterator extends WrappingIterator {

    private final SortedKeyValueIterator<Key, Value> sortedKeyValueIterator;

    WrappedIterator(SortedKeyValueIterator<Key, Value> sortedKeyValueIterator){
        this.sortedKeyValueIterator=sortedKeyValueIterator;
    }
    public void seek(Range range) throws IOException {
        Collection<ByteSequence> families = Collections.EMPTY_LIST;
        sortedKeyValueIterator.seek(range,families,false);
    }

    /***
     * wrapped methods
     */

    public Key getTopKey() {
        return sortedKeyValueIterator.getTopKey();
    }

    public Value getTopValue() {
        return sortedKeyValueIterator.getTopValue();
    }

    public boolean hasTop() {
        return sortedKeyValueIterator.hasTop();
    }


    public void next() throws IOException {
        sortedKeyValueIterator.next();
    }

    public void seek(Range range, Collection<ByteSequence> columnFamilies, boolean inclusive) throws IOException {
        seek(range);
    }

}
