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

#ifndef MUTATION_H_
#define MUTATION_H_

#include <sys/types.h>
#include <stdint.h>
#include <vector>
#include <stdio.h>
#include <string.h>

#include "../streaming/ByteOutputStream.h"
#include "../streaming/EndianTranslation.h"
#include "../streaming/DataOutputStream.h"

# define BSWAP_32(x)  ((((x) & 0x000000ff) << 24) |      \
                      (((x) & 0x0000ff00) << 8)  |      \
                      (((x) & 0x00ff0000) >> 8)  |      \
                      (((x) & 0xff000000) >> 24))
#define	BSWAP_64(x)     (((uint64_t)(x) << 56) | (((uint64_t)(x) << 40) & 0xff000000000000ULL) | (((uint64_t)(x) << 24) & 0xff0000000000ULL) | (((uint64_t)(x) << 8)  & 0xff00000000ULL) | (((uint64_t)(x) >> 8)  & 0xff000000ULL) | (((uint64_t)(x) >> 24) & 0xff0000ULL) | (((uint64_t)(x) >> 40) & 0xff00ULL) | ((uint64_t)(x)  >> 56))

namespace cclient {

namespace data {

/**
 * Key/Value Mutation
 **/
class Mutation {
 public:
  /**
   * Constructor
   * @param row row for the mutation
   **/
  explicit Mutation(const std::string &row);

  void putDelete(const std::string &cf, const std::string &cq, const std::string &cv, int64_t ts){
	  put(cf,cq,cv,ts,true);
  }

  void putDelete(const std::string &cf, const std::string &cq, const std::string &cv){
	  put(cf,cq,cv,9223372036854775807L,true);
  }

  void put(const std::string &cf, const std::string &cq, const std::string &cv, int64_t ts, bool deleted);

  void put(const std::string &cf, const std::string &cq, const std::string &cv, int64_t ts, bool deleted, uint8_t *value, uint64_t value_len);

  void put(const std::string &cf, const std::string &cq = "", const std::string &cv = "", int64_t ts = 0, const std::string &value="");
  virtual ~Mutation();
  std::string getRow() const {
    return mut_row;
  }

  int32_t size() {
    return entries;
  }

  std::pair<uint8_t*, size_t> getData() {
    return std::make_pair((uint8_t*) outStream->getByteArray(), outStream->getPos());
  }

  std::string getDataStr() const {
    return std::string(outStream->getByteArray(), outStream->getPos());
  }
 protected:

  Mutation();

  void write(const uint8_t *b, uint32_t length) {
    reserve(length);
    memcpy(&data.at(ptr), b, length);
    ptr += length;
  }

  void write(bool b) {
    reserve(1);
    if (b == true)
      data.at(ptr) = 1;
    else
      data.at(ptr) = 0;
    ptr++;
  }

  void writeLong(int64_t l) {
    reserve(8);

    int64_t v = BSWAP_64(l);
    write((uint8_t*) &v, sizeof(int64_t));
  }

  void writeInt(int32_t i) {
    reserve(4);
    int32_t v = BSWAP_32(i);
    write((uint8_t*) &v, sizeof(int32_t));
  }

  void reserve(uint32_t reserveSize) {
    data.resize(data.size() + reserveSize);
  }

  std::unique_ptr<cclient::data::streams::ByteOutputStream> outStream;
  std::unique_ptr<cclient::data::streams::EndianTranslationStream> endianStream;
  std::unique_ptr<cclient::data::streams::DataOutputStream> baseStream;

  std::string mut_row;

  uint32_t ptr;
  std::vector<uint8_t> data;

  int32_t entries;
  std::vector<uint8_t> values;

};
}
}
#endif /* MUTATION_H_ */

