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

#include "data/constructs/rfile/meta/IndexManager.h"

namespace cclient{
  namespace data{

IndexManager::IndexManager (cclient::data::compression::Compressor *compressorRef,cclient::data::streams::InputStream *blockReader, int version) :
    blockReader (blockReader), version (version), indexBlock (NULL), compressorRef (
        compressorRef)
{

}

uint64_t
IndexManager::read (cclient::data::streams::InputStream *in)
{
    size = 0;
    if (version == 6 || version == 7)
    {
        size = in->readInt ();
    }

    indexBlock = std::make_shared< IndexBlock> (version);
    indexBlock->read (in);

    if (version == 3 || version == 4)
    {
        size = indexBlock->getIndex ()->size ();

    }

    return in->getPos ();

}
  }
}
