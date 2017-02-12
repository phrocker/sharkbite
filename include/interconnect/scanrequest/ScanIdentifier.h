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
#ifndef SCAN_IDENT_H_
#define SCAN_IDENT_H_


#include <vector>
#include <algorithm>
#include <map>



namespace interconnect {
struct CopyKey {
    template<typename T>
    typename T::first_type operator()(T keyValuePair) const {
        return keyValuePair.first;
    }
};

/**
 * Scan identifier is a mechanism by which we can identify 
 * scans.
 * 
 **/
template<typename M, typename V>
class ScanIdentifier {
protected:
    std::map<M, std::vector<V> > globalIdentifierMapping;

public:
    ScanIdentifier() {

    }

    /**
     * Returns global identifiers for identifying scans
     * @param m key
     * @returns global identifiers for the provided key, m
     **/
    std::vector<V> getIdentifiers(M m) {
        return globalIdentifierMapping[m];
    }

    /**
     * Puts an identifier into the associated global map
     * @param m map key
     * @param v map key's value
     **/
    void putIdentifier(M m, V v) {
        globalIdentifierMapping[m].push_back(v);
    }

    /**
     * Returns a copy of all global mappings
     * @returns copied vector.
     **/
    std::vector<M> getGlobalMapping() {
        std::vector<M> keys;
        std::transform(globalIdentifierMapping.begin(),
                  globalIdentifierMapping.end(), std::back_inserter(keys), CopyKey());
        return keys;
    }
};
}
#endif
