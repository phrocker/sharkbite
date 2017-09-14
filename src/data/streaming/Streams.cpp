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
#include "../../../include/data/streaming/ByteOutputStream.h"
#include "../../../include/data/streaming/OutputStream.h"
#include "../../../include/data/streaming/Streams.h"

#include "../../../include/data/streaming/input/ByteInputStream.h"
#include "../../../include/data/streaming/input/InputStream.h"

/*
 */

namespace cclient{
  namespace data{
    namespace streams{
StreamInterface::StreamInterface ()
{

}

StreamInterface::~StreamInterface ()
{

}

std::shared_ptr<StreamInterface>
StreamInterface::getStream ()
{
    return NULL;
}

uint64_t
StreamInterface::write (OutputStream *out)
{
    return 0;
}

uint64_t
StreamInterface::read (InputStream *in)
{
    return 0;
}

    }
  }
}
