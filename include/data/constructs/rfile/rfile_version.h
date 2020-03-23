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
#ifndef RVERSION
#define RVERSION

#include "../../streaming/Streams.h"
#include "../../streaming/DataOutputStream.h"

namespace cclient {
namespace data {

#define VERSION_SIZE 4

class RFileVersion : public cclient::data::streams::StreamInterface {

 public:

  RFileVersion()
      :
      maj(0),
      min(0) {

  }

  /**
   * Constructor for RFile Version
   * @param majorV major version
   * @param minorV minor version
   */
  RFileVersion(const short majorV, const short minorV)
      :
      maj(majorV),
      min(minorV) {

  }

  explicit RFileVersion(cclient::data::streams::InputStream *input) {
    maj = input->readShort();
    min = input->readShort();
  }

  uint64_t read(cclient::data::streams::InputStream *input) {
    maj = input->readShort();
    min = input->readShort();
    return input->getPos();
  }

  /**
   * Write the RFile File Version
   * @param out output stream
   * @returns location of output stream
   */
  uint64_t write(cclient::data::streams::OutputStream *out) {

    out->writeShort(maj);
    return out->writeShort(min);
  }

  /**
   * Sets the major version
   * @param val major version
   */
  void setMajor(const short val) {

    maj = val;
  }

  /**
   * Gets the major version
   * @returns the major versions
   */
  short getMajor() {
    return maj;
  }

  /**
   * Sets the minor version
   * @param val minor version
   */
  void setMinor(const short val) {
    min = val;
  }

  /**
   * Returns the minor version
   * @returns minor version
   */
  short getMinor() {
    return min;
  }

 public:

  // major and minor versions.
  short maj, min;

};
}
}

#endif
