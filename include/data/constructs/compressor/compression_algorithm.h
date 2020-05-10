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
#ifndef COMPRESSION_ALGO
#define COMPRESSION_ALGO
#include <string>



#include "algorithm.h"


namespace cclient
{
namespace data
{
namespace compression
{

class CompressionAlgorithm : public Algorithm
{
public:
    CompressionAlgorithm () :
        Algorithm ("")
    {

    }

    explicit CompressionAlgorithm (const std::string compressionName) :
        Algorithm (compressionName)
    {

    }

    explicit CompressionAlgorithm (const Algorithm &other) :
        Algorithm (other.getAlgorithm())
    {

    }


    CompressionAlgorithm(const CompressionAlgorithm &other) = default;

    CompressionAlgorithm(CompressionAlgorithm &&other) = default;

    CompressionAlgorithm &
    operator= (const Algorithm &other)
    {
        this->setAlgorithm( other.getAlgorithm() );
        return *this;
    }

    CompressionAlgorithm &
    operator= (const CompressionAlgorithm &other) = default;

    CompressionAlgorithm &
    operator= (CompressionAlgorithm &&other) = default;

};
}
}
}
#endif

