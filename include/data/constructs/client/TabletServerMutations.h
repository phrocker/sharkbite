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
#ifndef TABLETSERVERMUTATIONS_H_
#define TABLETSERVERMUTATIONS_H_

#include <map>


#include "../KeyExtent.h"
#include "../Mutation.h"


namespace cclient {
namespace data {

class TabletServerMutations {
public:
    explicit TabletServerMutations(std::string sessionId);
    TabletServerMutations(std::string sessionId, uint32_t fails);
    virtual ~TabletServerMutations();

    void addMutation(KeyExtent extent, Mutation* m) {
        std::vector<Mutation*> &mutationList = mutations[extent];
        mutationList.push_back(m);
    }

    std::map<KeyExtent, std::vector<Mutation*> > *getMutations() {
        return &mutations;
    }
    
    void setMaxFailures(uint32_t failures)
    {
      failuresAllowed=failures;
    }
    
    uint32_t getMaxFailures()
    {
      return failuresAllowed;
    }

protected:
    uint32_t failuresAllowed;
    std::map<KeyExtent, std::vector<Mutation*> > mutations;
    std::string session;

};

} /* namespace data */
} /* namespace cclient */
#endif /* TABLETSERVERMUTATIONS_H_ */
