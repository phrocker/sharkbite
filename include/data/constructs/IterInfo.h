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

#ifndef ITERINFO_H_
#define ITERINFO_H_

#include <string>
#include <map>
#include "inputvalidation.h"

namespace cclient
{
namespace data
{
  
enum class ITERATOR_TYPES
{
    majc=1, 
    minc=2, 
    scan=4
    
};

typedef Iterator<ITERATOR_TYPES, ITERATOR_TYPES::majc, ITERATOR_TYPES::scan> TypeIterator;


inline ITERATOR_TYPES operator|(ITERATOR_TYPES a, ITERATOR_TYPES b)
{return static_cast<ITERATOR_TYPES>(static_cast<int>(a) | static_cast<int>(b));}


inline std::ostream& operator << (std::ostream& out, const ITERATOR_TYPES& obj)
{
    switch(obj)
    {
      case ITERATOR_TYPES::majc:
	out << "majc";
	break;
      case ITERATOR_TYPES::minc:
	out << "minc";
	break;
      case ITERATOR_TYPES::scan:
	out << "majc";
	break;
	
	
    };
    return out;
}

/**
 * Stores iterator information
 **/
class IterInfo
{
public:
    /**
     * Constructor 
     * @param name iterator name
     * @param cl iterator class name
     * @param pri iterator priority
     **/
    IterInfo (std::string name, std::string cl, uint32_t pri) :
        iterName (name), iterClass (cl), priority (pri)
    {

    }
    /**
     * Destructor
     **/
    virtual
    ~IterInfo ()
    {

    }

    /**
     * Returns the priority
     * @returns priority of this iterator
     **/
    uint32_t
    getPriority () const
    {
        return priority;
    }

    /**
     * Returns the iterator name
     * @returns name of this iterator
     **/
    std::string
    getName () const
    {
        return iterName;
    }

    /**
     * Returns the class
     * @returns classname of this iterator
     **/
    std::string
    getClass () const
    {
        return iterClass;
    }

    /**
     * Adds an option to the iterator map
     * @param optionName name of the option
     * @param option value
     * @returns result
     **/
    bool
    addOption (std::string optionName, std::string optionValue)
    {
       if (!IsEmpty(&optionName) || !IsEmpty(&optionValue))
	{
	  return false;
	}
        options[optionName] = optionValue;
       return true;
    }

    /**
     * Returns options
     * @returns options for this iterator
     **/
    const std::map<std::string, std::string>
    getOptions () const
    {
        return options;
    }
protected:

    // options map
    std::map<std::string,std::string> options;
    // priority for this iterator
    uint32_t priority;
    // name for this iterator
    std::string iterName;
    // canonical class name
    std::string iterClass;
};

} /* namespace data */
} /* namespace cclient */
#endif /* ITERINFO_H_ */
