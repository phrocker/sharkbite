package org.poma.accumulo;

import org.apache.accumulo.core.data.ArrayByteSequence;
import org.apache.accumulo.core.data.ByteSequence;
import org.apache.accumulo.core.data.KeyBuilder;
import org.apache.accumulo.core.data.KeyValue;
import org.apache.accumulo.core.data.PartialKey;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.dataImpl.thrift.TKey;
import org.apache.accumulo.core.dataImpl.thrift.TKeyValue;
import org.apache.accumulo.core.security.ColumnVisibility;
import org.apache.accumulo.core.util.ByteBufferUtil;
import org.apache.hadoop.io.Text;
import org.apache.hadoop.io.WritableComparable;
import org.apache.hadoop.io.WritableComparator;
import org.apache.hadoop.io.WritableUtils;

import java.io.DataInput;
import java.io.DataOutput;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.util.Arrays;
import java.util.List;

public class Key implements WritableComparable<Key>, Cloneable{
    protected byte[] row;
    protected byte[] colFamily;
    protected byte[] colQualifier;
    protected byte[] colVisibility;
    protected long timestamp;
    protected boolean deleted;
    private static final byte[] EMPTY_BYTES = new byte[0];


    public boolean equals(Object o) {
        return o instanceof Key ? this.equals((Key)o, PartialKey.ROW_COLFAM_COLQUAL_COLVIS_TIME_DEL) : false;
    }

    static byte[] copyIfNeeded(byte[] ba, int off, int len, boolean copyData) {
        if (len == 0) {
            return EMPTY_BYTES;
        } else if (!copyData && ba.length == len && off == 0) {
            return ba;
        } else {
            byte[] copy = new byte[len];
            System.arraycopy(ba, off, copy, 0, len);
            return copy;
        }
    }

    private final void init(byte[] r, int rOff, int rLen, byte[] cf, int cfOff, int cfLen, byte[] cq, int cqOff, int cqLen, byte[] cv, int cvOff, int cvLen, long ts, boolean del, boolean copy) {
        this.row = copyIfNeeded(r, rOff, rLen, copy);
        this.colFamily = copyIfNeeded(cf, cfOff, cfLen, copy);
        this.colQualifier = copyIfNeeded(cq, cqOff, cqLen, copy);
        this.colVisibility = copyIfNeeded(cv, cvOff, cvLen, copy);
        this.timestamp = ts;
        this.deleted = del;
    }

    public Key(org.apache.accumulo.core.data.Key key) {
        set(key);
    }

    public Key() {
        this.row = EMPTY_BYTES;
        this.colFamily = EMPTY_BYTES;
        this.colQualifier = EMPTY_BYTES;
        this.colVisibility = EMPTY_BYTES;
        this.timestamp = 9223372036854775807L;
        this.deleted = false;
    }

    public Key(Text row) {
        this.init(row.getBytes(), 0, row.getLength(), EMPTY_BYTES, 0, 0, EMPTY_BYTES, 0, 0, EMPTY_BYTES, 0, 0, 9223372036854775807L, false, true);
    }

    public Key(byte[] row) {
        this.init(row, 0, row.length, EMPTY_BYTES, 0, 0, EMPTY_BYTES, 0, 0, EMPTY_BYTES, 0, 0, 9223372036854775807L, false, true);
    }

    public Key(Text row, long ts) {
        this(row);
        this.timestamp = ts;
    }

    public Key(byte[] row, long ts) {
        this(row);
        this.timestamp = ts;
    }

    public Key(byte[] row, int rOff, int rLen, byte[] cf, int cfOff, int cfLen, byte[] cq, int cqOff, int cqLen, byte[] cv, int cvOff, int cvLen, long ts) {
        this.init(row, rOff, rLen, cf, cfOff, cfLen, cq, cqOff, cqLen, cv, cvOff, cvLen, ts, false, true);
    }

    Key(byte[] row, int rOff, int rLen, byte[] cf, int cfOff, int cfLen, byte[] cq, int cqOff, int cqLen, byte[] cv, int cvOff, int cvLen, long ts, boolean deleted, boolean copy) {
        this.init(row, rOff, rLen, cf, cfOff, cfLen, cq, cqOff, cqLen, cv, cvOff, cvLen, ts, deleted, copy);
    }

    public Key(byte[] row, byte[] colFamily, byte[] colQualifier, byte[] colVisibility, long timestamp) {
        this(row, colFamily, colQualifier, colVisibility, timestamp, false, true);
    }

    public Key(byte[] row, byte[] cf, byte[] cq, byte[] cv, long ts, boolean deleted) {
        this(row, cf, cq, cv, ts, deleted, true);
    }

    public Key(byte[] row, byte[] cf, byte[] cq, byte[] cv, long ts, boolean deleted, boolean copy) {
        this.init(row, 0, row.length, cf, 0, cf.length, cq, 0, cq.length, cv, 0, cv.length, ts, deleted, copy);
    }

    public Key(Text row, Text cf) {
        this.init(row.getBytes(), 0, row.getLength(), cf.getBytes(), 0, cf.getLength(), EMPTY_BYTES, 0, 0, EMPTY_BYTES, 0, 0, 9223372036854775807L, false, true);
    }

    public Key(byte[] row, byte[] cf) {
        this.init(row, 0, row.length, cf, 0, cf.length, EMPTY_BYTES, 0, 0, EMPTY_BYTES, 0, 0, 9223372036854775807L, false, true);
    }

    public Key(Text row, Text cf, Text cq) {
        this.init(row.getBytes(), 0, row.getLength(), cf.getBytes(), 0, cf.getLength(), cq.getBytes(), 0, cq.getLength(), EMPTY_BYTES, 0, 0, 9223372036854775807L, false, true);
    }

    public Key(byte[] row, byte[] cf, byte[] cq) {
        this.init(row, 0, row.length, cf, 0, cf.length, cq, 0, cq.length, EMPTY_BYTES, 0, 0, 9223372036854775807L, false, true);
    }

    public Key(Text row, Text cf, Text cq, Text cv) {
        this.init(row.getBytes(), 0, row.getLength(), cf.getBytes(), 0, cf.getLength(), cq.getBytes(), 0, cq.getLength(), cv.getBytes(), 0, cv.getLength(), 9223372036854775807L, false, true);
    }

    public Key(byte[] row, byte[] cf, byte[] cq, byte[] cv) {
        this.init(row, 0, row.length, cf, 0, cf.length, cq, 0, cq.length, cv, 0, cv.length, 9223372036854775807L, false, true);
    }

    public Key(Text row, Text cf, Text cq, long ts) {
        this.init(row.getBytes(), 0, row.getLength(), cf.getBytes(), 0, cf.getLength(), cq.getBytes(), 0, cq.getLength(), EMPTY_BYTES, 0, 0, ts, false, true);
    }

    public Key(byte[] row, byte[] cf, byte[] cq, long ts) {
        this.init(row, 0, row.length, cf, 0, cf.length, cq, 0, cq.length, EMPTY_BYTES, 0, 0, ts, false, true);
    }

    public Key(Text row, Text cf, Text cq, Text cv, long ts) {
        this.init(row.getBytes(), 0, row.getLength(), cf.getBytes(), 0, cf.getLength(), cq.getBytes(), 0, cq.getLength(), cv.getBytes(), 0, cv.getLength(), ts, false, true);
    }

    public Key(Text row, Text cf, Text cq, ColumnVisibility cv, long ts) {
        byte[] expr = cv.getExpression();
        this.init(row.getBytes(), 0, row.getLength(), cf.getBytes(), 0, cf.getLength(), cq.getBytes(), 0, cq.getLength(), expr, 0, expr.length, ts, false, true);
    }

    public Key(byte[] row, byte[] cf, byte[] cq, ColumnVisibility cv, long ts) {
        byte[] expr = cv.getExpression();
        this.init(row, 0, row.length, cf, 0, cf.length, cq, 0, cq.length, expr, 0, expr.length, ts, false, true);
    }

    public Key(CharSequence row) {
        this(new Text(row.toString()));
    }

    public Key(CharSequence row, CharSequence cf) {
        this(new Text(row.toString()), new Text(cf.toString()));
    }

    public Key(CharSequence row, CharSequence cf, CharSequence cq) {
        this(new Text(row.toString()), new Text(cf.toString()), new Text(cq.toString()));
    }

    public Key(CharSequence row, CharSequence cf, CharSequence cq, CharSequence cv) {
        this(new Text(row.toString()), new Text(cf.toString()), new Text(cq.toString()), new Text(cv.toString()));
    }

    public Key(CharSequence row, CharSequence cf, CharSequence cq, long ts) {
        this(new Text(row.toString()), new Text(cf.toString()), new Text(cq.toString()), ts);
    }

    public Key(CharSequence row, CharSequence cf, CharSequence cq, CharSequence cv, long ts) {
        this(new Text(row.toString()), new Text(cf.toString()), new Text(cq.toString()), new Text(cv.toString()), ts);
    }

    public Key(CharSequence row, CharSequence cf, CharSequence cq, ColumnVisibility cv, long ts) {
        this(new Text(row.toString()), new Text(cf.toString()), new Text(cq.toString()), new Text(cv.getExpression()), ts);
    }

    private byte[] followingArray(byte[] ba) {
        byte[] fba = new byte[ba.length + 1];
        System.arraycopy(ba, 0, fba, 0, ba.length);
        fba[ba.length] = 0;
        return fba;
    }

    public Key followingKey(PartialKey part) {
        Key returnKey = new Key();
        switch(part) {
            case ROW:
                returnKey.row = this.followingArray(this.row);
                break;
            case ROW_COLFAM:
                returnKey.row = this.row;
                returnKey.colFamily = this.followingArray(this.colFamily);
                break;
            case ROW_COLFAM_COLQUAL:
                returnKey.row = this.row;
                returnKey.colFamily = this.colFamily;
                returnKey.colQualifier = this.followingArray(this.colQualifier);
                break;
            case ROW_COLFAM_COLQUAL_COLVIS:
                returnKey.row = this.row;
                returnKey.colFamily = this.colFamily;
                returnKey.colQualifier = this.colQualifier;
                returnKey.colVisibility = this.followingArray(this.colVisibility);
                break;
            case ROW_COLFAM_COLQUAL_COLVIS_TIME:
                returnKey.row = this.row;
                returnKey.colFamily = this.colFamily;
                returnKey.colQualifier = this.colQualifier;
                returnKey.colVisibility = this.colVisibility;
                returnKey.setTimestamp(this.timestamp - 1L);
                returnKey.deleted = false;
                break;
            default:
                throw new IllegalArgumentException("Partial key specification " + part + " disallowed");
        }

        return returnKey;
    }

    public Key(Key other) {
        this.set(other);
    }

    public Key(TKey tkey) {
        this.row = ByteBufferUtil.toBytes(tkey.row);
        this.colFamily = ByteBufferUtil.toBytes(tkey.colFamily);
        this.colQualifier = ByteBufferUtil.toBytes(tkey.colQualifier);
        this.colVisibility = ByteBufferUtil.toBytes(tkey.colVisibility);
        this.timestamp = tkey.timestamp;
        this.deleted = false;
        if (this.row == null) {
            throw new IllegalArgumentException("null row");
        } else if (this.colFamily == null) {
            throw new IllegalArgumentException("null column family");
        } else if (this.colQualifier == null) {
            throw new IllegalArgumentException("null column qualifier");
        } else if (this.colVisibility == null) {
            throw new IllegalArgumentException("null column visibility");
        }
    }

    public String getRow(Text r) {
        r.set(this.row, 0, this.row.length);
        return r.toString();
    }

    public ByteSequence getRowData() {
        return new ArrayByteSequence(this.row);
    }

    public String getRow() {
        return this.getRow(new Text()).toString();
    }

    public int compareRow(Text r) {
        return WritableComparator.compareBytes(this.row, 0, this.row.length, r.getBytes(), 0, r.getLength());
    }

    public ByteSequence getColumnFamilyData() {
        return new ArrayByteSequence(this.colFamily);
    }

    public String getColumnFamily(Text cf) {
        cf.set(this.colFamily, 0, this.colFamily.length);
        return cf.toString();
    }

    public String getColumnFamily() {
        return this.getColumnFamily(new Text()).toString();
    }

    public int compareColumnFamily(Text cf) {
        return WritableComparator.compareBytes(this.colFamily, 0, this.colFamily.length, cf.getBytes(), 0, cf.getLength());
    }

    public ByteSequence getColumnQualifierData() {
        return new ArrayByteSequence(this.colQualifier);
    }

    public String getColumnQualifier(Text cq) {
        cq.set(this.colQualifier, 0, this.colQualifier.length);
        return cq.toString();
    }

    public String getColumnQualifier() {
        return this.getColumnQualifier(new Text()).toString();
    }

    public int compareColumnQualifier(Text cq) {
        return WritableComparator.compareBytes(this.colQualifier, 0, this.colQualifier.length, cq.getBytes(), 0, cq.getLength());
    }

    public void setTimestamp(long ts) {
        this.timestamp = ts;
    }

    public long getTimestamp() {
        return this.timestamp;
    }

    public boolean isDeleted() {
        return this.deleted;
    }

    public void setDeleted(boolean deleted) {
        this.deleted = deleted;
    }

    public ByteSequence getColumnVisibilityData() {
        return new ArrayByteSequence(this.colVisibility);
    }

    public final String getColumnVisibility() {
        return this.getColumnVisibility(new Text()).toString();
    }

    public final String getColumnVisibility(Text cv) {
        cv.set(this.colVisibility, 0, this.colVisibility.length);
        return cv.toString();
    }

    public final ColumnVisibility getColumnVisibilityParsed() {
        return new ColumnVisibility(this.colVisibility);
    }

    public void set(Key k) {
        this.row = k.row;
        this.colFamily = k.colFamily;
        this.colQualifier = k.colQualifier;
        this.colVisibility = k.colVisibility;
        this.timestamp = k.timestamp;
        this.deleted = k.deleted;
    }

    public void set(org.apache.accumulo.core.data.Key k) {
        this.row = k.getRow().copyBytes();
        this.colFamily = k.getColumnFamily().copyBytes();
        this.colQualifier = k.getColumnQualifier().copyBytes();
        this.colVisibility = k.getColumnVisibility().copyBytes();
        this.timestamp = k.getTimestamp();
        this.deleted = k.isDeleted();
    }

    public void readFields(DataInput in) throws IOException {
        int colFamilyOffset = WritableUtils.readVInt(in);
        int colQualifierOffset = WritableUtils.readVInt(in);
        int colVisibilityOffset = WritableUtils.readVInt(in);
        int totalLen = WritableUtils.readVInt(in);
        this.row = new byte[colFamilyOffset];
        this.colFamily = new byte[colQualifierOffset - colFamilyOffset];
        this.colQualifier = new byte[colVisibilityOffset - colQualifierOffset];
        this.colVisibility = new byte[totalLen - colVisibilityOffset];
        in.readFully(this.row);
        in.readFully(this.colFamily);
        in.readFully(this.colQualifier);
        in.readFully(this.colVisibility);
        this.timestamp = WritableUtils.readVLong(in);
        this.deleted = in.readBoolean();
    }

    public void write(DataOutput out) throws IOException {
        int colFamilyOffset = this.row.length;
        int colQualifierOffset = colFamilyOffset + this.colFamily.length;
        int colVisibilityOffset = colQualifierOffset + this.colQualifier.length;
        int totalLen = colVisibilityOffset + this.colVisibility.length;
        WritableUtils.writeVInt(out, colFamilyOffset);
        WritableUtils.writeVInt(out, colQualifierOffset);
        WritableUtils.writeVInt(out, colVisibilityOffset);
        WritableUtils.writeVInt(out, totalLen);
        out.write(this.row);
        out.write(this.colFamily);
        out.write(this.colQualifier);
        out.write(this.colVisibility);
        WritableUtils.writeVLong(out, this.timestamp);
        out.writeBoolean(this.deleted);
    }

    public boolean equals(Key other, PartialKey part) {
        switch(part) {
            case ROW:
                return isEqual(this.row, other.row);
            case ROW_COLFAM:
                return isEqual(this.row, other.row) && isEqual(this.colFamily, other.colFamily);
            case ROW_COLFAM_COLQUAL:
                return isEqual(this.row, other.row) && isEqual(this.colFamily, other.colFamily) && isEqual(this.colQualifier, other.colQualifier);
            case ROW_COLFAM_COLQUAL_COLVIS:
                return isEqual(this.row, other.row) && isEqual(this.colFamily, other.colFamily) && isEqual(this.colQualifier, other.colQualifier) && isEqual(this.colVisibility, other.colVisibility);
            case ROW_COLFAM_COLQUAL_COLVIS_TIME:
                return isEqual(this.row, other.row) && isEqual(this.colFamily, other.colFamily) && isEqual(this.colQualifier, other.colQualifier) && isEqual(this.colVisibility, other.colVisibility) && this.timestamp == other.timestamp;
            case ROW_COLFAM_COLQUAL_COLVIS_TIME_DEL:
                return isEqual(this.row, other.row) && isEqual(this.colFamily, other.colFamily) && isEqual(this.colQualifier, other.colQualifier) && isEqual(this.colVisibility, other.colVisibility) && this.timestamp == other.timestamp && this.deleted == other.deleted;
            default:
                throw new IllegalArgumentException("Unrecognized partial key specification " + part);
        }
    }

    public int compareTo(Key other, PartialKey part) {
        int result = WritableComparator.compareBytes(this.row, 0, this.row.length, other.row, 0, other.row.length);
        if (result == 0 && !part.equals(PartialKey.ROW)) {
            result = WritableComparator.compareBytes(this.colFamily, 0, this.colFamily.length, other.colFamily, 0, other.colFamily.length);
            if (result == 0 && !part.equals(PartialKey.ROW_COLFAM)) {
                result = WritableComparator.compareBytes(this.colQualifier, 0, this.colQualifier.length, other.colQualifier, 0, other.colQualifier.length);
                if (result == 0 && !part.equals(PartialKey.ROW_COLFAM_COLQUAL)) {
                    result = WritableComparator.compareBytes(this.colVisibility, 0, this.colVisibility.length, other.colVisibility, 0, other.colVisibility.length);
                    if (result == 0 && !part.equals(PartialKey.ROW_COLFAM_COLQUAL_COLVIS)) {
                        if (this.timestamp < other.timestamp) {
                            result = 1;
                        } else if (this.timestamp > other.timestamp) {
                            result = -1;
                        } else {
                            result = 0;
                        }

                        if (result == 0 && !part.equals(PartialKey.ROW_COLFAM_COLQUAL_COLVIS_TIME)) {
                            if (this.deleted) {
                                result = other.deleted ? 0 : -1;
                            } else {
                                result = other.deleted ? 1 : 0;
                            }

                            return result;
                        } else {
                            return result;
                        }
                    } else {
                        return result;
                    }
                } else {
                    return result;
                }
            } else {
                return result;
            }
        } else {
            return result;
        }
    }

    public org.apache.accumulo.core.data.Key toAccumuloKey(){
        return new org.apache.accumulo.core.data.Key(row,colFamily,colQualifier,colVisibility,timestamp,deleted,true);
    }

    public int compareTo(Key other) {
        return this.compareTo(other, PartialKey.ROW_COLFAM_COLQUAL_COLVIS_TIME_DEL);
    }

    public int hashCode() {
        return WritableComparator.hashBytes(this.row, this.row.length) + WritableComparator.hashBytes(this.colFamily, this.colFamily.length) + WritableComparator.hashBytes(this.colQualifier, this.colQualifier.length) + WritableComparator.hashBytes(this.colVisibility, this.colVisibility.length) + (int)(this.timestamp ^ this.timestamp >>> 32);
    }

    public static String toPrintableString(byte[] ba, int offset, int len, int maxLen) {
        return appendPrintableString(ba, offset, len, maxLen, new StringBuilder()).toString();
    }

    public static StringBuilder appendPrintableString(byte[] ba, int offset, int len, int maxLen, StringBuilder sb) {
        int plen = Math.min(len, maxLen);

        for(int i = 0; i < plen; ++i) {
            int c = 255 & ba[offset + i];
            if (c >= 32 && c <= 126) {
                sb.append((char)c);
            } else {
                sb.append("%" + String.format("%02x;", c));
            }
        }

        if (len > maxLen) {
            sb.append("... TRUNCATED");
        }

        return sb;
    }

    private StringBuilder rowColumnStringBuilder() {
        return this.rowColumnStringBuilder(64);
    }

    private StringBuilder rowColumnStringBuilder(int maxComponentLength) {
        StringBuilder sb = new StringBuilder();
        appendPrintableString(this.row, 0, this.row.length, maxComponentLength, sb);
        sb.append(" ");
        appendPrintableString(this.colFamily, 0, this.colFamily.length, maxComponentLength, sb);
        sb.append(":");
        appendPrintableString(this.colQualifier, 0, this.colQualifier.length, maxComponentLength, sb);
        sb.append(" [");
        appendPrintableString(this.colVisibility, 0, this.colVisibility.length, maxComponentLength, sb);
        sb.append("]");
        return sb;
    }

    public String toString() {
        StringBuilder sb = this.rowColumnStringBuilder();
        sb.append(" ");
        sb.append(Long.toString(this.timestamp));
        sb.append(" ");
        sb.append(this.deleted);
        return sb.toString();
    }

    public String toStringNoTruncate() {
        StringBuilder sb = this.rowColumnStringBuilder(2147483647);
        sb.append(" ");
        sb.append(Long.toString(this.timestamp));
        sb.append(" ");
        sb.append(this.deleted);
        return sb.toString();
    }

    public String toStringNoTime() {
        return this.rowColumnStringBuilder().toString();
    }

    public int getLength() {
        return this.row.length + this.colFamily.length + this.colQualifier.length + this.colVisibility.length;
    }

    public int getSize() {
        return this.getLength();
    }

    private static boolean isEqual(byte[] a1, byte[] a2) {
        if (a1 == a2) {
            return true;
        } else {
            int last = a1.length;
            if (last != a2.length) {
                return false;
            } else if (last == 0) {
                return true;
            } else {
                --last;
                if (a1[last] == a2[last]) {
                    for(int i = 0; i < last; ++i) {
                        if (a1[i] != a2[i]) {
                            return false;
                        }
                    }

                    return true;
                } else {
                    return false;
                }
            }
        }
    }

    byte[] getRowBytes() {
        return this.row;
    }

    byte[] getColFamily() {
        return this.colFamily;
    }

    byte[] getColQualifier() {
        return this.colQualifier;
    }

    byte[] getColVisibility() {
        return this.colVisibility;
    }

    public Object clone() throws CloneNotSupportedException {
        Key r = (Key)super.clone();
        r.row = Arrays.copyOf(this.row, this.row.length);
        r.colFamily = Arrays.copyOf(this.colFamily, this.colFamily.length);
        r.colQualifier = Arrays.copyOf(this.colQualifier, this.colQualifier.length);
        r.colVisibility = Arrays.copyOf(this.colVisibility, this.colVisibility.length);
        return r;
    }
    static byte[] copy(byte[] ba, int off, int len, boolean copyData) {
        if (len == 0) {
            return EMPTY_BYTES;
        } else if (!copyData && ba.length == len && off == 0) {
            return ba;
        } else {
            byte[] copy = new byte[len];
            System.arraycopy(ba, off, copy, 0, len);
            return copy;
        }
    }

    public void setColumnQualifier(String cq){
        Text cqT = new Text(cq);
        this.colFamily = copy(cqT.getBytes(), 0, cqT.getLength(), true);
    }


    public void setColumnFamily(String cf){
        Text cfT = new Text(cf);
        this.colFamily = copy(cfT.getBytes(), 0, cfT.getLength(), true);
    }

}
