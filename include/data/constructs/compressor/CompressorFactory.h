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

#ifndef COMPRESSOR_FACTORY_H
#define COMPRESSOR_FACTORY_H

#include "algorithm.h"
#include "compressor.h"
#include "zlibCompressor.h"
#include "compression_algorithm.h"
#include <memory>

namespace cclient
{
    namespace data
    {
        namespace compression
        {
            class CompressorFactory
            {
            public:
                static ZLibCompressor *getZlibCompressor()
                {
                    static ZLibCompressor *comp = new ZLibCompressor();
                    return comp;
                }

                static std::unique_ptr<Compressor> create(const CompressionAlgorithm &algo)
                {
                    if (algo.getAlgorithm() == "gz")
                    {

                        return std::make_unique<ZLibCompressor>();
                    }
                    else
                    {
                        std::runtime_error("Unsupported compression algorithm");
                    }
                    return 0;
                }
            };
        } // namespace compression
    }     // namespace data
} // namespace cclient

#endif