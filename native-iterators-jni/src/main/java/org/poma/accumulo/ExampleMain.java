package org.poma.accumulo;

import org.apache.accumulo.core.data.ByteSequence;
import org.apache.accumulo.core.data.Key;
import org.apache.accumulo.core.data.Range;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.iterators.IteratorEnvironment;
import org.apache.accumulo.core.iterators.SortedKeyValueIterator;
import org.apache.accumulo.core.iterators.SortedMapIterator;

import java.io.File;
import java.io.IOException;
import java.util.Collection;
import java.util.Collections;
import java.util.HashMap;
import java.util.Map;
import java.util.Scanner;
import java.util.SortedMap;
import java.util.TreeMap;

public class ExampleMain {

    public static void main(String [] args) throws IOException {

        String path = args[0];
        long ts = System.currentTimeMillis();
        NativeDSLIterator iter = new NativeDSLIterator();
        System.out.println("Time taken " + (System.currentTimeMillis()-ts));

        ts = System.currentTimeMillis();
        iter = new NativeDSLIterator();
        System.out.println("Time taken " + (System.currentTimeMillis()-ts));


        SortedMap<Key, Value> map = new TreeMap<>();

        map.put(new Key("arow"),new Value());
        map.put(new Key("bbrow"),new Value());
        map.put(new Key("c"),new Value());

        SortedMapIterator smi = new SortedMapIterator(map);

        Map<String,String> options =  new HashMap<>();


        String content = new Scanner(new File(path)).useDelimiter("\\Z").next();


        options.put(NativeDSLIterator.DSL_TYPE,"Python");
        options.put(NativeDSLIterator.DSL_VALUE,content);

        System.out.println("Script is " + content);

        iter.init(smi, options, new IteratorEnvironment() {
            @Override
            public SortedKeyValueIterator<Key, Value> reserveMapFileReader(String mapFileName) throws IOException {
                return null;
            }
        });

        Collection<ByteSequence> sequences = Collections.emptyList();
        iter.seek(new Range(), sequences,true);

        iter.next();


    }
}
