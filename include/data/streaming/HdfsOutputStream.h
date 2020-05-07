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
#include "Streams.h"
#include "extern/libhdfs3/client/hdfs.h"
#include "utils/Uri.h"
#include "data/constructs/client/Hdfs.h"
#include "OutputStream.h"
#include "ByteOutputStream.h"

namespace cclient {
namespace data {
namespace streams {

class HadoopDataOutputStream: public DataOutputStream {
public:


    explicit HadoopDataOutputStream(OutputStream *out_stream)  : DataOutputStream(out_stream), output_stream_ref( out_stream )
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

protected:
    // output stream reference.
    cclient::data::streams::OutputStream *output_stream_ref;

};


class HdfsOutputStream: public ByteOutputStream {
public:

    explicit HdfsOutputStream(const std::string path);

    explicit HdfsOutputStream(const std::shared_ptr<hdfs::HdfsLink> &hdfs, const std::string &path);

    ~HdfsOutputStream(){
        hdfsCloseFile(hdfs->getHdfsReference(), fileRef);
    }

   /**
     * Copied from
     * http://stackoverflow.com/questions/3022552/is-there-any-standard-htonl-like-function-for-64-bits-integers-in-c
     */
    uint64_t htonlw(uint64_t value) {
        // The answer is 42
        static const int num = 42;

        // Check the endianness
        if (*reinterpret_cast<const char*>(&num) == num) {
            const uint32_t high_part = htonl(
                                           static_cast<uint32_t>(value >> 32));
            const uint32_t low_part = htonl(
                                          static_cast<uint32_t>(value & 0xFFFFFFFFLL));

            return (static_cast<uint64_t>(low_part) << 32) | high_part;
        } else {
            return value;
        }
    }

    virtual uint64_t writeShort(short shortVal) override {

        short sht = htons(shortVal);
        return writeBytes(reinterpret_cast<uint8_t*>(&sht),2);
    }

    virtual uint64_t writeInt(int intVal) override{

       int ntv = htonl(intVal);
       return writeBytes(reinterpret_cast<uint8_t*>(&ntv),4);
    }

    virtual uint64_t writeLong(uint64_t val) override {

        uint64_t vl = htonlw(val);
        return writeBytes(reinterpret_cast<uint8_t*>(&vl),8);
    }

    virtual uint64_t writeBytes(const char *bytes, size_t cnt) override{
        written += cnt;
        return hdfsWrite(hdfs->getHdfsReference(),fileRef,static_cast<const void*>(bytes),cnt);
    }

    
    virtual uint64_t writeBytes(const uint8_t *bytes, size_t cnt) override {
        written += cnt;
        return hdfsWrite(hdfs->getHdfsReference(),fileRef,static_cast<const void*>(bytes),cnt);
    }

    
    virtual uint64_t write(const uint8_t *bytes, long cnt) override{
        written += cnt;
        return hdfsWrite(hdfs->getHdfsReference(),fileRef,static_cast<const void*>(bytes),cnt);
    }

    
    virtual uint64_t getPos() override{
        return written;
    }

    uint64_t write(const char *bytes, long cnt) override{
        written += cnt;
        return hdfsWrite(hdfs->getHdfsReference(),fileRef,static_cast<const void*>(bytes),cnt);
    }

    virtual uint64_t writeByte(int byte) override {
        return writeBytes(reinterpret_cast<uint8_t*>(&byte),1);
    }




    virtual uint64_t writeByte(const uint8_t byte) override {
        return writeBytes(&byte,1);
    }

protected:

  size_t size;
  hdfsFile fileRef;
  std::string file;
  size_t written;

    // output stream reference. 
  std::shared_ptr<hdfs::HdfsLink> hdfs;


};


}
}
}
#endif
