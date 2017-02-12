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

#include <hdfs/hdfs.h>
#include "../../../include/data/streaming/HdfsStream.h"

namespace cclient
{
namespace data
{
namespace streams
{

HdfsStream::HdfsStream (hdfsFS fs, const char *path, const int flags,
                        const int bufferSize, short replication,
                        tOffset blockSize) :
    fs (fs)
{
    file = hdfsOpenFile (fs, path, flags, bufferSize, replication,
                         blockSize);
    amountWritten = 0;
}

HdfsStream::~HdfsStream ()
{
    hdfsCloseFile (fs, file);
}

uint64_t
HdfsStream::getPos ()
{
    return amountWritten;
}

uint64_t
HdfsStream::write (const char *bytes, long cnt)
{
    amountWritten += hdfsWrite (fs, file, bytes, cnt);
    return amountWritten;
}

uint64_t
HdfsStream::writeByte (int byte)
{
    hdfsWrite (fs, file, (const char*) &byte, 1);
    return ++amountWritten;
}

uint64_t
HdfsStream::writeString (std::string s)
{
    writeHadoopLong (s.size ());
    return writeBytes ((uint8_t*) s.data (), s.size ());
}

uint64_t
HdfsStream::write (const uint8_t *bytes, long cnt)
{
    amountWritten += hdfsWrite (fs, file, bytes, cnt);
    return amountWritten;
}

uint64_t
HdfsStream::writeBytes (const uint8_t *bytes, size_t cnt)
{
    amountWritten += hdfsWrite (fs, file, bytes, cnt);
    return amountWritten;
}

uint64_t
HdfsStream::writeByte (const uint8_t byte)
{
    hdfsWrite (fs, file, (const char*) &byte, 1);
    return ++amountWritten;
}

uint64_t
HdfsStream::writeShort (const short shortVal)
{

    return write ((const char*) &shortVal, 2);
}
uint64_t
HdfsStream::writeInt (const int intVal)
{
    return write ((const char*) &intVal, 4);

}

uint64_t
HdfsStream::writeLong (const uint64_t val)
{
    return write ((const char*) &val, 8);

}

uint64_t
HdfsStream::writeBoolean (const bool val)
{
    uint8_t byte = 0x00;
    if (val)
        byte = 0x01;
    return write ((const char*) &byte, 1);
}

int main(int argc, char **argv)
{
  struct hdfsBuilder *builder = hdfsNewBuilder ();
  hdfsFreeBuilder(builder);
}



} /* namespace streams */
} /* namespace data */
} /* namespace cclient */

