#include "data/constructs/rfile/RFileReader.h"

namespace cclient{
namespace data{

RFile *open(std::ifstream &ifs){

    cclient::data::streams::InputStream *stream = new cclient::data::streams::InputStream(&ifs,0);

    stream = new cclient::data::streams::EndianInputStream(stream);

    std::fstream::pos_type size = filesize(outputFile.c_str());
    
    return new cclient::data::RFile (stream, size);
}

}

}