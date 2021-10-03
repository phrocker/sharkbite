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
#ifndef TRANSLATION_STREAM
#define TRANSLATION_STREAM

#include "DataOutputStream.h"
#include "HdfsOutputStream.h"
#include "OutputStream.h"

namespace cclient {
namespace data {
namespace streams {

class EndianTranslationStream : public HadoopDataOutputStream {
 public:
  explicit EndianTranslationStream(OutputStream *os)
      : HadoopDataOutputStream(os) {}

  explicit EndianTranslationStream(std::unique_ptr<OutputStream> os)
      : HadoopDataOutputStream(os.get()), ref(std::move(os)) {
  }

  EndianTranslationStream(EndianTranslationStream &&) = delete;

  EndianTranslationStream &operator=(EndianTranslationStream &&) = delete;

  virtual ~EndianTranslationStream();

  uint64_t htonlw(uint64_t value);

  uint64_t writeShort(short shortVal) override;

  uint64_t writeInt(int intVal);

  uint64_t writeLong(uint64_t val);

  virtual uint64_t writeEncodedLong(const int64_t n);

 protected:
  virtual bool canReclaim() { return ref == nullptr; }

 private:
  std::unique_ptr<OutputStream> ref;
};

}  // namespace streams
}  // namespace data
}  // namespace cclient
#endif
