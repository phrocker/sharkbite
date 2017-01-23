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

#include "../../../../../include/data/constructs/rfile/bcfile/BlockRegion.h"

namespace cclient{
  namespace data{
uint64_t
BlockRegion::read (streams::InputStream *in)
{
    offset = in->readHadoopLong ();
    compressedSize = in->readHadoopLong ();
    rawSize = in->readHadoopLong ();
    return in->getPos ();
}

uint64_t
BlockRegion::write (streams::OutputStream *out)
{
    
    out->writeEncodedLong (offset);
    out->writeEncodedLong (compressedSize);
    uint64_t pos = out->writeEncodedLong(rawSize);
    return pos;

}

  }
}