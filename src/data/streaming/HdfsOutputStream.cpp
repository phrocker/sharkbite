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
    : ByteOutputStream(0), hdfs(nullptr), file(path) {
  utils::Uri uri(path);

  hdfs = std::make_shared<hdfs::HdfsLink>(file, uri.port());

  file = uri.path();

  fileRef = hdfsOpenFile(hdfs->getHdfsReference(), file.c_str(),
                         O_WRONLY | O_APPEND, 0, 0, 0);
  auto ret = hdfsGetPathInfo(hdfs->getHdfsReference(), file.c_str());
  if (!fileRef || !ret) {
    throw cclient::exceptions::HDFSException("File does not exist");
  }
  size = ret->mSize;
  hdfsFreeFileInfo(ret, 1);
}

HdfsOutputStream::HdfsOutputStream(const std::shared_ptr<hdfs::HdfsLink> &hdfs,
                                   const std::string &path)
    : ByteOutputStream(0), hdfs(hdfs), file(path) {
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
    throw cclient::exceptions::HDFSException("File does not exist");
  }
  size = ret->mSize;
  hdfsFreeFileInfo(ret, 1);
}

}  // namespace streams
}  // namespace data
}  // namespace cclient
