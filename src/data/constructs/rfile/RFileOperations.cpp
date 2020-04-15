#include "data/constructs/rfile/RFileOperations.h"

namespace cclient{
namespace data{



cclient::data::RFile *RFileOperations::open(const std::string &rfile){

    auto ifs = std::make_unique<std::ifstream >(rfile.c_str(), std::ifstream::binary | std::ifstream::in);

    cclient::data::streams::InputStream *stream = new cclient::data::streams::InputStream(std::move(ifs),0);

    stream = new cclient::data::streams::EndianInputStream(stream);

    std::fstream::pos_type size = filesize(rfile.c_str());

    return new cclient::data::RFile (stream, size);
}

cclient::data::SequentialRFile *RFileOperations::openSequential(const std::string &rfile){

    auto ifs = std::make_unique<std::ifstream >(rfile.c_str(), std::ifstream::binary | std::ifstream::in);

    cclient::data::streams::InputStream *stream = new cclient::data::streams::InputStream(std::move(ifs),0);

    stream = new cclient::data::streams::EndianInputStream(stream);

    std::fstream::pos_type size = filesize(rfile.c_str());
    
    return new cclient::data::SequentialRFile (stream, size);
}

std::ifstream::pos_type RFileOperations::filesize(const char* filename)
    {
        std::ifstream in(filename, std::ifstream::ate | std::ifstream::binary);
        return in.tellg();
    }


}
}
