/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#ifndef DATA_OUT_STREAM
#define DATA_OUT_STREAM

#include <cstdint>
#include <iostream>
#include "OutputStream.h"

namespace cclient {
namespace data {
namespace streams {

class DataOutputStream: public OutputStream {
public:
    /**
     Constructor.
     @param output stream.
     **/
    explicit DataOutputStream(OutputStream *out_stream) :
        OutputStream(out_stream), output_stream_ref(out_stream) {

    }

    virtual ~DataOutputStream() {

    }

    virtual uint64_t getPos() {
        return output_stream_ref->getPos();
    }

    uint64_t write(const char *bytes, long cnt) {
        return output_stream_ref->write(bytes, cnt);
    }

    virtual uint64_t writeByte(int byte) {
        return output_stream_ref->writeByte(byte);
    }

    virtual uint64_t writeString(std::string s) {
        // write size of string
        output_stream_ref->writeVLong(s.size());
        return output_stream_ref->writeBytes((uint8_t*) s.data(), s.size());
    }

    virtual uint64_t write(const uint8_t *bytes, long cnt) {
        return output_stream_ref->write(bytes, cnt);
    }

    virtual uint64_t writeBytes(const char *bytes, size_t cnt) {
            return output_stream_ref->writeBytes(bytes, cnt);
        }

    virtual uint64_t writeBytes(const uint8_t *bytes, size_t cnt) {
        return output_stream_ref->writeBytes(bytes, cnt);
    }

    virtual uint64_t writeByte(const uint8_t byte) {
        return output_stream_ref->writeByte(byte);
    }

    virtual uint64_t writeShort(const short shortVal) {
        return output_stream_ref->writeShort(shortVal);
    }
    virtual uint64_t writeInt(const int intVal) {
        return output_stream_ref->writeInt(intVal);
    }

    virtual uint64_t writeLong(const uint64_t val) {
        return output_stream_ref->writeLong(val);
    }

    virtual uint64_t writeBoolean(const bool val) {

        return output_stream_ref->writeBoolean(val);
    }

    virtual uint64_t writeEncodedLong(const int64_t n) {
        return output_stream_ref->writeEncodedLong(n);
    }

    virtual uint64_t writeVLong(const int64_t n) {
        int64_t i = n;
        if (i >= -112L && i <= 127L) {
            output_stream_ref->write((const char*) &i, 1);
        } else {
            int len = -112;
            if (i < 0L) {
                i = ~i;
                len = -120;
            }

            for (long tmp = i; tmp != 0L; --len) {
                tmp >>= 8;
            }
            char b = (char) len;
            output_stream_ref->write((const char*) &b, 1);
            len = len < -120 ? -(len + 120) : -(len + 112);

            for (int idx = len; idx != 0; --idx) {
                int shiftbits = (idx - 1) * 8;
                long mask = 255L << shiftbits;
                char val = (char) ((int) ((i & mask) >> shiftbits));
                output_stream_ref->write((const char*) &val, 1);
            }

        }

        return output_stream_ref->getPos();

    }

protected:
    // output stream reference.
    OutputStream *output_stream_ref;

};
}
}
}
#endif
