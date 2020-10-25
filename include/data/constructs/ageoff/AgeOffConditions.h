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
#pragma once
#include <cstdlib>
#include <iostream>
#include <vector>
#include <map>
namespace cclient{
namespace data{


enum AgeOffType{
    DATA_TYPE,
    DEFAULT
};

struct AgeOffCondition{
    AgeOffCondition():type(AgeOffType::DEFAULT),name("default"),earliest_allowed_timestamp(0){
    }
    explicit AgeOffCondition(AgeOffType type, std::string name, int64_t earliest_allowed_timestamp) : type(type),name(name),earliest_allowed_timestamp(earliest_allowed_timestamp){}
    AgeOffType type;
    std::string name;
    int64_t earliest_allowed_timestamp;
};

class AgeOffEvaluator{
private:
    std::multimap<AgeOffType,AgeOffCondition> conditionals;
    AgeOffCondition default_condition;
public:
    AgeOffEvaluator(){
    }
    virtual ~AgeOffEvaluator(){}


    explicit AgeOffEvaluator(AgeOffCondition condition){
        default_condition.type=AgeOffType::DEFAULT;
        default_condition.name="default";
        default_condition.earliest_allowed_timestamp=0;
        
        if (condition.type==AgeOffType::DEFAULT){
            if (default_condition.earliest_allowed_timestamp != 0){
                throw std::runtime_error("Cannot have more than one default condition");
            }
            default_condition.earliest_allowed_timestamp = condition.earliest_allowed_timestamp;
        }
        else{
            conditionals.insert(std::make_pair(condition.type,condition));
        }

        
    }

    explicit AgeOffEvaluator(std::vector<AgeOffCondition> conditions){
        default_condition.type=AgeOffType::DEFAULT;
        default_condition.name="default";
        default_condition.earliest_allowed_timestamp=0;
        for(const auto &condition : conditions){
            if (condition.type==AgeOffType::DEFAULT){
                if (default_condition.earliest_allowed_timestamp != 0){
                    throw std::runtime_error("Cannot have more than one default condition");
                }
                default_condition.earliest_allowed_timestamp = condition.earliest_allowed_timestamp;
                continue;
            }
            conditionals.insert(std::make_pair(condition.type,condition));

        }
    }

    virtual bool filtered(const std::shared_ptr<Key> &key){
        if (default_condition.earliest_allowed_timestamp > 0){
            // filter if the timestamp is before the earliest_allowed_timestamp 
            return key->getTimeStamp() < default_condition.earliest_allowed_timestamp;
        }
        return false;
    }

    std::vector<AgeOffCondition> getConditions(AgeOffType type) const{
        const auto range = conditionals.equal_range(type);
        std::vector<AgeOffCondition> conditions;
        std::transform(range.first, range.second,std::back_inserter(conditions), [](std::pair<AgeOffType,AgeOffCondition> element){return element.second;});
        return conditions;
    }

    AgeOffCondition getDefaultCondition() const{
        return default_condition;
    }

    bool empty() const{
        return conditionals.empty() && default_condition.earliest_allowed_timestamp==0;
    }

};


}
}