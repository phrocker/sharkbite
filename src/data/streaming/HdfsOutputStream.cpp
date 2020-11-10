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

#include "data/streaming/HdfsOutputStream.h"

#include <iostream>

#include "data/exceptions/HDFSException.h"
#include "data/streaming/OutputStream.h"

namespace cclient {
namespace data {
namespace streams {
HadoopDataOutputStream::~HadoopDataOutputStream() {}

uint64_t HadoopDataOutputStream::getPos() {
  return output_stream_ref->getPos();
}

uint64_t HadoopDataOutputStream::write(const char *bytes, long cnt) {
  return output_stream_ref->write(bytes, cnt);
}

uint64_t HadoopDataOutputStream::writeByte(int byte) {
  return output_stream_ref->writeByte(byte);
}

uint64_t HadoopDataOutputStream::writeString(std::string s) {
  // write size of string
  output_stream_ref->writeHadoopLong(s.size());
  return output_stream_ref->writeBytes((uint8_t *)s.data(), s.size());
}

uint64_t HadoopDataOutputStream::write(const uint8_t *bytes, long cnt) {
  return output_stream_ref->write(bytes, cnt);
}

uint64_t HadoopDataOutputStream::writeBytes(const uint8_t *bytes, size_t cnt) {
  return output_stream_ref->writeBytes(bytes, cnt);
}

uint64_t HadoopDataOutputStream::writeByte(const uint8_t byte) {
  return output_stream_ref->writeByte(byte);
}

uint64_t HadoopDataOutputStream::writeShort(const short shortVal) {
  return output_stream_ref->writeShort(shortVal);
}
uint64_t HadoopDataOutputStream::writeInt(const int intVal) {
  return output_stream_ref->writeInt(intVal);
}

uint64_t HadoopDataOutputStream::writeLong(const uint64_t val) {
  return output_stream_ref->writeLong(val);
}

uint64_t HadoopDataOutputStream::writeBoolean(const bool val) {
  return output_stream_ref->writeBoolean(val);
}

uint64_t HadoopDataOutputStream::writeHadoopLong(const int64_t n) {
  if ((n < 128) && (n >= -32)) {
    return output_stream_ref->writeByte((int)n);
  } else {
  }

  long un = (n < 0) ? ~n : n;
  // how many bytes do we need to represent the number with sign bit?
  int len = (64 - output_stream_ref->numberOfLeadingZeros(un)) / 8 + 1;
  int firstByte = (int)(n >> ((len - 1) * 8));
  switch (len) {
    case 1:
      // fall it through to firstByte==-1, len=2.
      firstByte >>= 8;
    case 2:
      if ((firstByte < 20) && (firstByte >= -20)) {
        output_stream_ref->writeByte(firstByte - 52);
        return output_stream_ref->writeByte((int)n);
      }
      // fall it through to firstByte==0/-1, len=3.
      firstByte >>= 8;
    case 3:
      if ((firstByte < 16) && (firstByte >= -16)) {
        output_stream_ref->writeByte(firstByte - 88);
        return output_stream_ref->writeShort((int)n);
      }
      // fall it through to firstByte==0/-1, len=4.
      firstByte >>= 8;
    case 4:
      if ((firstByte < 8) && (firstByte >= -8)) {
        output_stream_ref->writeByte(firstByte - 112);
        output_stream_ref->writeShort(((int)n) >> 8);
        return output_stream_ref->writeByte((int)n);
      }
      output_stream_ref->writeByte(len - 129);
      return output_stream_ref->writeInt((int)n);
    case 5:
      output_stream_ref->writeByte(len - 129);
      output_stream_ref->writeInt((int)(n >> 8));
      return output_stream_ref->writeByte((int)n);
    case 6:
      output_stream_ref->writeByte(len - 129);
      output_stream_ref->writeInt((int)(n >> 16));
      return output_stream_ref->writeShort((int)n);
    case 7:
      output_stream_ref->writeByte(len - 129);
      output_stream_ref->writeInt((int)(n >> 24));
      output_stream_ref->writeShort((int)(n >> 8));
      return output_stream_ref->writeByte((int)n);
    case 8:
      output_stream_ref->writeByte(len - 129);
      return output_stream_ref->writeLong(n);
    default:
      return output_stream_ref->getPos();
  };
  return output_stream_ref->getPos();
}

HdfsOutputStream::HdfsOutputStream(const std::string path)
    : hdfs(nullptr), file(path) {
  utils::Uri uri(path);

  hdfs = std::make_shared<hdfs::HdfsLink>(uri.host(), uri.port());

  file = uri.path();

  fileRef = hdfsOpenFile(hdfs->getHdfsReference(), file.c_str(),
                         O_WRONLY | O_SYNC, 0, 1, 0);
  auto ret = hdfsGetPathInfo(hdfs->getHdfsReference(), file.c_str());
  if (!fileRef || !ret) {
    throw cclient::exceptions::HDFSException("File does not exist on write");
  }
  size = ret->mSize;
  hdfsFreeFileInfo(ret, 1);
}

HdfsOutputStream::HdfsOutputStream(const std::shared_ptr<hdfs::HdfsLink> &hdfs,
                                   const std::string &path)
    : hdfs(hdfs), file(path) {
  try {
    utils::Uri uri(path);

    file = uri.path();
  } catch (...) {
    file = path;
  }

  fileRef = hdfsOpenFile(hdfs->getHdfsReference(), file.c_str(),
                         O_WRONLY | O_APPEND, 0, 0, 0);
  auto ret = hdfsGetPathInfo(hdfs->getHdfsReference(), file.c_str());
  if (!fileRef || !ret) {
    throw cclient::exceptions::HDFSException("File does not exist on write");
  }
  size = ret->mSize;
  hdfsFreeFileInfo(ret, 1);
}

uint64_t HdfsOutputStream::htonlw(uint64_t value) {
  // The answer is 42
  static const int num = 42;

  // Check the endianness
  if (*reinterpret_cast<const char *>(&num) == num) {
    const uint32_t high_part = htonl(static_cast<uint32_t>(value >> 32));
    const uint32_t low_part =
        htonl(static_cast<uint32_t>(value & 0xFFFFFFFFLL));

    return (static_cast<uint64_t>(low_part) << 32) | high_part;
  } else {
    return value;
  }
}

uint64_t HdfsOutputStream::writeVLong(const int64_t initial) {
  int64_t n = initial;
  int64_t i = n;
  if (i >= -112L && i <= 127L) {
    write((const char *)&i, 1);
  } else {
    int len = -112;
    if (i < 0L) {
      i = ~i;
      len = -120;
    }

    for (long tmp = i; tmp != 0L; --len) {
      tmp >>= 8;
    }
    char b = (char)len;
    write((const char *)&b, 1);
    len = len < -120 ? -(len + 120) : -(len + 112);

    for (int idx = len; idx != 0; --idx) {
      int shiftbits = (idx - 1) * 8;
      long mask = 255L << shiftbits;
      char val = (char)((int)((i & mask) >> shiftbits));
      write((const char *)&val, 1);
      // stream.writeByte((byte)((int)((i & mask) >> shiftbits)));
    }
  }

  return getPos();
}

uint64_t HdfsOutputStream::writeString(std::string s) {
  // write size of string
  // writeHadoopLong(htonlw(s.size()));
  writeHadoopLong(s.size());
  return writeBytes((uint8_t *)s.data(), s.size());
}

uint64_t HdfsOutputStream::writeShort(short shortVal) {
  short sht = htons(shortVal);
  return writeBytes(reinterpret_cast<uint8_t *>(&sht), 2);
}

uint64_t HdfsOutputStream::writeInt(int intVal) {
  int ntv = htonl(intVal);
  return writeBytes(reinterpret_cast<uint8_t *>(&ntv), 4);
}

uint64_t HdfsOutputStream::writeLong(uint64_t val) {
  uint64_t vl = htonlw(val);
  return writeBytes(reinterpret_cast<uint8_t *>(&vl), 8);
}

uint64_t HdfsOutputStream::writeByte(int byte) {
  return writeBytes((uint8_t *)&byte, 1);
}

uint64_t HdfsOutputStream::writeBoolean(const bool val) {
  uint8_t byte = 0x00;
  if (val) byte = 0x01;
  return writeBytes((const char *)&byte, 1);
}
uint64_t HdfsOutputStream::writeBytes(const char *bytes, size_t cnt) {
  position += cnt;
  return hdfsWrite(hdfs->getHdfsReference(), fileRef,
                   static_cast<const void *>(bytes), cnt);
}

uint64_t HdfsOutputStream::writeBytes(const uint8_t *bytes, size_t cnt) {
  position += cnt;
  return hdfsWrite(hdfs->getHdfsReference(), fileRef,
                   static_cast<const void *>(bytes), cnt);
}

uint64_t HdfsOutputStream::write(const uint8_t *bytes, long cnt) {
  position += cnt;
  return hdfsWrite(hdfs->getHdfsReference(), fileRef,
                   static_cast<const void *>(bytes), cnt);
}

uint64_t HdfsOutputStream::write(const char *bytes, long cnt) {
  position += cnt;
  return hdfsWrite(hdfs->getHdfsReference(), fileRef,
                   static_cast<const void *>(bytes), cnt);
}

uint64_t HdfsOutputStream::writeEncodedLong(const int64_t n) {
  // int64_t n = htonlw(initial);
  if ((n < 128) && (n >= -32)) {
    write((const char *)&n, 1);
    return position;
    // return writeByte((int) n);
  }

  long un = (n < 0) ? ~n : n;
  int temp = n;
  // how many bytes do we need to represent the number with sign bit?
  int len = (64 - numberOfLeadingZeros(un)) / 8 + 1;
  int firstByte = (int)(n >> ((len - 1) * 8));
  switch (len) {
    case 1:
      // fall it through to firstByte==-1, len=2.
      firstByte >>= 8;
    case 2:
      if ((firstByte < 20) && (firstByte >= -20)) {
        temp = firstByte - 52;
        write((const char *)&temp, 1);
        write((const char *)&n, 1);
        return position;
        // writeByte(firstByte - 52);
        // return writeByte((int) n);
      }
      // fall it through to firstByte==0/-1, len=3.
      firstByte >>= 8;
    case 3:
      if ((firstByte < 16) && (firstByte >= -16)) {
        temp = firstByte - 88;
        write((const char *)&temp, 1);
        write((const char *)((short *)&n), 2);
        // writeByte(firstByte - 88);
        // return writeShort((int) n);
      }
      // fall it through to firstByte==0/-1, len=4.
      firstByte >>= 8;
    case 4:
      if ((firstByte < 8) && (firstByte >= -8)) {
        /*
         writeByte(firstByte - 112);
         writeShort(((int) n) >> 8);
         return writeByte((int) n);
         */
        temp = firstByte - 112;
        write((const char *)&temp, 1);
        temp = n >> 8;
        write((const char *)((short *)&temp), 2);
        // ostream_ref->write((const char*)((short*)&n),2);
        write((const char *)&n, 1);
        return position;
      }
      temp = len - 129;
      write((const char *)&temp, 1);
      write((const char *)&n, 4);
      return position;
      // writeByte(len - 129);

      /// return writeInt((int) n);
    case 5:
      temp = len - 129;
      write((const char *)&temp, 1);
      temp = n >> 8;
      write((const char *)&temp, 4);
      write((const char *)&n, 1);
      return position;
      /*
       writeByte(len - 129);
       writeInt((int) (n >> 8));
       return writeByte((int) n);
       */
    case 6:
      temp = len - 129;
      write((const char *)&temp, 1);
      temp = n >> 16;
      write((const char *)&temp, 4);
      write((const char *)((short *)&n), 2);
      return position;
      /*
       writeByte(len - 129);
       writeInt((int) (n >> 16));
       return writeShort((int) n);
       */
    case 7:
      temp = len - 129;
      write((const char *)&temp, 1);
      temp = n >> 24;
      write((const char *)&temp, 4);
      temp = n >> 8;
      write((const char *)((short *)&temp), 2);
      write((const char *)((short *)&n), 1);
      return position;
      /*
       writeByte(len - 129);
       writeInt((int) (n >> 24));
       writeShort((int) (n >> 8));
       return writeByte((int) n);
       */
    case 8:
      temp = len - 129;
      write((const char *)&temp, 1);

      write((const char *)&n, 8);
      return position;
      /*
       writeByte(len - 129);
       return writeLong(n);
       */
    default:
      return position;
  };
  return position;
}

uint64_t HdfsOutputStream::writeHadoopLong(const int64_t n) {
  if ((n < 128) && (n >= -32)) {
    return writeByte((int)n);
  } else {
  }

  long un = (n < 0) ? ~n : n;
  // how many bytes do we need to represent the number with sign bit?
  int len = (64 - numberOfLeadingZeros(un)) / 8 + 1;
  int firstByte = (int)(n >> ((len - 1) * 8));
  switch (len) {
    case 1:
      // fall it through to firstByte==-1, len=2.
      firstByte >>= 8;
    case 2:
      if ((firstByte < 20) && (firstByte >= -20)) {
        writeByte(firstByte - 52);
        return writeByte((int)n);
      }
      // fall it through to firstByte==0/-1, len=3.
      firstByte >>= 8;
    case 3:
      if ((firstByte < 16) && (firstByte >= -16)) {
        writeByte(firstByte - 88);
        return writeShort((int)n);
      }
      // fall it through to firstByte==0/-1, len=4.
      firstByte >>= 8;
    case 4:
      if ((firstByte < 8) && (firstByte >= -8)) {
        writeByte(firstByte - 112);
        writeShort(((int)n) >> 8);
        return writeByte((int)n);
      }
      writeByte(len - 129);
      return writeInt((int)n);
    case 5:
      writeByte(len - 129);
      writeInt((int)(n >> 8));
      return writeByte((int)n);
    case 6:
      writeByte(len - 129);
      writeInt((int)(n >> 16));
      return writeShort((int)n);
    case 7:
      writeByte(len - 129);
      writeInt((int)(n >> 24));
      writeShort((int)(n >> 8));
      return writeByte((int)n);
    case 8:
      writeByte(len - 129);
      return writeLong(n);
    default:
      return getPos();
  };
  return getPos();
}

}  // namespace streams
}  // namespace data
}  // namespace cclient
