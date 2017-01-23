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
#ifndef HADOOP_DOUT_STREAM
#define HADOOP_DOUT_STREAM

#include <cstdint>



#include "OutputStream.h"

namespace cclient {
namespace data {
namespace streams {

class HadoopDataOutputStream: public OutputStream {
public:


    HadoopDataOutputStream(OutputStream *out_stream)  : OutputStream(out_stream), output_stream_ref( out_stream )
    {

    }

    virtual ~HadoopDataOutputStream();
    virtual uint64_t getPos();
    uint64_t write(const char *bytes, long cnt);
    virtual uint64_t writeByte(int byte);
    virtual uint64_t writeString(std::string s);
    virtual uint64_t write(const uint8_t *bytes, long cnt);
    virtual uint64_t writeBytes(const uint8_t *bytes, size_t cnt);
    virtual uint64_t writeByte(const uint8_t byte);
    virtual uint64_t writeShort(const short shortVal);
    virtual uint64_t writeInt(const int intVal);
    virtual uint64_t writeLong(const uint64_t val);
    virtual uint64_t writeBoolean(const bool val);
    virtual uint64_t writeHadoopLong(const int64_t n);
    OutputStream* outputStream;

protected:
    // output stream reference.
    cclient::data::streams::OutputStream *output_stream_ref;

};
}
}
}
#endif
