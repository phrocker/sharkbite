package pysharkbite;

import org.apache.accumulo.core.data.Value;

import java.nio.ByteBuffer;
import java.util.Map;

public class KeyValue implements Map.Entry<Key, Value> {

    Key ref;
    Value valueRef;

    public KeyValue(Key key, byte[] value) {
        ref=key;
        valueRef = new Value(value, false);
    }

    public KeyValue(Key key, ByteBuffer value) {
        ref=key;
        valueRef = new Value(value);
    }

    public KeyValue(Key key, Value value) {
        ref=key;
        valueRef = new Value(value);
    }

    @Override
    public Key getKey() {
        return ref;
    }

    @Override
    public Value getValue() {
        return valueRef;
    }

    @Override
    public Value setValue(Value value) {
        valueRef = new Value(value);
        return valueRef;
    }

    @Override
    public boolean equals(Object o) {
        return false;
    }

    @Override
    public int hashCode() {
        return ref.hashCode();
    }
}
