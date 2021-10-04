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

#include "data/constructs/Mutation.h"

#include "data/constructs/../streaming/ByteOutputStream.h"
#include "data/constructs/../streaming/EndianTranslation.h"
namespace cclient {

namespace data {

Mutation::Mutation(const std::string &row)
    : mut_row(row),
      ptr(0),
      entries(0),
      outStream(std::make_unique<streams::ByteOutputStream>(1024)) {
  endianStream =
      std::make_unique<streams::EndianTranslationStream>(outStream.get());
  baseStream = std::make_unique<streams::DataOutputStream>(endianStream.get());
}

Mutation::~Mutation() {}

void Mutation::put(const std::string &cf, const std::string &cq,
                   const std::string &cv, int64_t ts, bool deleted,
                   uint8_t *value, uint64_t value_len) {
  baseStream->writeVLong(cf.size());
  // writeInt(cf.size());

  baseStream->write((uint8_t *)cf.c_str(), cf.size());
  // write((uint8_t*)cf.c_str(),cf.size());
  baseStream->writeVLong(cq.size());
  // writeInt(cq.size());
  baseStream->write((uint8_t *)cq.c_str(), cq.size());
  // write((uint8_t*)cq.c_str(),cq.size());
  baseStream->writeVLong(cv.size());
  //	writeInt(cv.size());
  baseStream->write((uint8_t *)cv.c_str(), cv.size());
  // write((uint8_t*)cv.c_str(),cv.size());
  baseStream->writeBoolean(true);
  // write(true);
  baseStream->writeVLong(ts);
  // writeLong(ts);
  baseStream->writeBoolean(deleted);
  // write(false);
  baseStream->writeVLong(value_len);
  // writeInt(value_len);
  baseStream->write(value, value_len);
  // write(value,value_len);
  entries++;
}

void Mutation::put(const std::string &cf, const std::string &cq,
                   const std::string &cv, int64_t ts, bool deleted) {
  baseStream->writeVLong(cf.size());
  // writeInt(cf.size());

  baseStream->write((uint8_t *)cf.c_str(), cf.size());
  // write((uint8_t*)cf.c_str(),cf.size());
  baseStream->writeVLong(cq.size());
  // writeInt(cq.size());
  baseStream->write((uint8_t *)cq.c_str(), cq.size());
  // write((uint8_t*)cq.c_str(),cq.size());
  baseStream->writeVLong(cv.size());
  //	writeInt(cv.size());
  baseStream->write((uint8_t *)cv.c_str(), cv.size());
  // write((uint8_t*)cv.c_str(),cv.size());
  baseStream->writeBoolean(true);
  // write(true);
  baseStream->writeVLong(ts);
  // writeLong(ts);
  baseStream->writeBoolean(deleted);
  // write(false);
  baseStream->writeVLong(0);

  entries++;
}

void Mutation::put(const std::string &cf, const std::string &cq) {
  baseStream->writeVLong(cf.size());
  // writeInt(cf.size());

  baseStream->write((uint8_t *)cf.c_str(), cf.size());
  // write((uint8_t*)cf.c_str(),cf.size());
  baseStream->writeVLong(cq.size());
  // writeInt(cq.size());
  if (!cq.empty()) baseStream->write((uint8_t *)cq.c_str(), cq.size());
  // write((uint8_t*)cq.c_str(),cq.size());
  baseStream->writeVLong(0);
  //  writeInt(cv.size());

  baseStream->writeBoolean(false);

  baseStream->writeBoolean(false);

  baseStream->writeVLong(0);

  entries++;
}

void Mutation::put(const std::string &cf, const std::string &cq,
                   const std::string &cv) {
  baseStream->writeVLong(cf.size());
  // writeInt(cf.size());

  baseStream->write((uint8_t *)cf.c_str(), cf.size());
  // write((uint8_t*)cf.c_str(),cf.size());
  baseStream->writeVLong(cq.size());
  // writeInt(cq.size());
  if (!cq.empty()) baseStream->write((uint8_t *)cq.c_str(), cq.size());
  // write((uint8_t*)cq.c_str(),cq.size());
  baseStream->writeVLong(cv.size());
  //  writeInt(cv.size());
  if (!cv.empty()) baseStream->write((uint8_t *)cv.c_str(), cv.size());
  // no ts
  baseStream->writeBoolean(false);

  // no delete
  baseStream->writeBoolean(false);

  baseStream->writeVLong(0);

  entries++;
}

void Mutation::put(const std::string &cf, const std::string &cq,
                   const std::string &cv, int64_t ts) {
  baseStream->writeVLong(cf.size());
  // writeInt(cf.size());

  baseStream->write((uint8_t *)cf.c_str(), cf.size());
  // write((uint8_t*)cf.c_str(),cf.size());
  baseStream->writeVLong(cq.size());
  // writeInt(cq.size());
  if (!cq.empty()) baseStream->write((uint8_t *)cq.c_str(), cq.size());
  // write((uint8_t*)cq.c_str(),cq.size());
  baseStream->writeVLong(cv.size());
  //  writeInt(cv.size());
  if (!cv.empty()) baseStream->write((uint8_t *)cv.c_str(), cv.size());
  // write((uint8_t*)cv.c_str(),cv.size());
  if (ts == 0) {
    baseStream->writeBoolean(false);
  } else {
    baseStream->writeBoolean(true);
    // write(true);
    baseStream->writeVLong(ts);
  }
  // writeLong(ts);
  baseStream->writeBoolean(false);
  // write(false);

  baseStream->writeVLong(0);

  entries++;
}

void Mutation::put(const std::string &cf, const std::string &cq,
                   const std::string &cv, int64_t ts,
                   const std::string &value) {
  baseStream->writeVLong(cf.size());
  // writeInt(cf.size());

  baseStream->write((uint8_t *)cf.c_str(), cf.size());
  // write((uint8_t*)cf.c_str(),cf.size());
  baseStream->writeVLong(cq.size());
  // writeInt(cq.size());
  if (!cq.empty()) baseStream->write((uint8_t *)cq.c_str(), cq.size());
  // write((uint8_t*)cq.c_str(),cq.size());
  baseStream->writeVLong(cv.size());
  //	writeInt(cv.size());
  if (!cv.empty()) baseStream->write((uint8_t *)cv.c_str(), cv.size());
  // write((uint8_t*)cv.c_str(),cv.size());
  baseStream->writeBoolean(true);
  // write(true);
  baseStream->writeVLong(ts);
  // writeLong(ts);
  baseStream->writeBoolean(false);
  // write(false);

  if (!value.empty()) {
    baseStream->writeVLong(value.size());
    baseStream->write((uint8_t *)value.data(), value.size());
  } else {
    baseStream->writeVLong(0);
  }

  entries++;
}

}  // namespace data
}  // namespace cclient
