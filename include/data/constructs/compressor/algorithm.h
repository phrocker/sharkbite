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
#ifndef INCLUDE_DATA_CONSTRUCTS_COMPRESSOR_ALGORITHM_H_
#define INCLUDE_DATA_CONSTRUCTS_COMPRESSOR_ALGORITHM_H_
#include <string>

#ifndef SH_UNLIKELY
    #ifdef __GNUC__
    #define SH_UNLIKELY(val) (__builtin_expect((val), 0))
    #define SH_LIKELY(val) (__builtin_expect((val), 1))
    #else
    #define SH_UNLIKELY(val) (val)
    #define SH_LIKELY(val) (val)
    #endif
#endif


namespace cclient
{
namespace data
{
namespace compression
{

class Algorithm
{
public:
    Algorithm () :
        compressionAlgo ("")
    {

    }

    explicit Algorithm (const std::string &compressionName) :
        compressionAlgo (compressionName)
    {

    }

    Algorithm(const Algorithm &other) = default;

    Algorithm(Algorithm &&other) = default;

    /**
     * Set the algorithm name
     * @param algorithm algorithm name
     */
    void
    setAlgorithm (const std::string &algorithm)
    {
        compressionAlgo = algorithm;
    }

    /**
     * Get the algorithm name
     * @returns algorith name
     */
    std::string
    getName ()
    {
        return compressionAlgo;
    }

    Algorithm &
    operator= (const Algorithm &other) = default;

    Algorithm &
    operator= (Algorithm &&other) = default;

    std::string getAlgorithm() const
    {
        return compressionAlgo;
    }
    

protected:
    std::string compressionAlgo;

};
}
}
}
#endif /* INCLUDE_DATA_CONSTRUCTS_COMPRESSOR_ALGORITHM_H_ */
